package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/horizon/pkg/config"
	"github.com/hashicorp/horizon/pkg/control"
	"github.com/hashicorp/horizon/pkg/pb"
	"github.com/hashicorp/horizon/pkg/periodic"
	"github.com/hashicorp/horizon/pkg/tlsmanage"
	"github.com/hashicorp/horizon/pkg/workq"
	"github.com/hashicorp/vault/api"
	"github.com/jinzhu/gorm"
	"github.com/mitchellh/cli"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"io/ioutil"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"
)

type controlServer struct{}
func controlFactory() (cli.Command, error) {
	return &controlServer{}, nil
}

func (c *controlServer) Help() string {
	return "Start a control server"
}

func (c *controlServer) Synopsis() string {
	return "Start a control server"
}

func (c *controlServer) Run(args []string) int {
	level := hclog.Info
	if os.Getenv("DEBUG") != "" {
		level = hclog.Trace
	}

	L := hclog.New(&hclog.LoggerOptions{
		Name:  "control",
		Level: level,
		Exclude: hclog.ExcludeFuncs{
			hclog.ExcludeByPrefix("http: TLS handshake error from").Exclude,
		}.Exclude,
	})

	L.Info("log level configured", "level", level)
	L.Trace("starting server")

	vaultCfg := api.DefaultConfig()
	vaultClient, err := api.NewClient(vaultCfg)
	if err != nil {
		log.Fatal(err)
	}

	// If we have token AND this is kubernetes, then let's try to get a token
	if vaultClient.Token() == "" {
		f, err := os.Open("/var/run/secrets/kubernetes.io/serviceaccount/token")
		if err == nil {
			L.Info("attempting to login to vault via kubernetes auth")

			data, err := ioutil.ReadAll(f)
			if err != nil {
				log.Fatal(err)
			}

			f.Close()

			sec, err := vaultClient.Logical().Write("auth/kubernetes/login", map[string]interface{}{
				"role": "horizon",
				"jwt":  string(bytes.TrimSpace(data)),
			})
			if err != nil {
				log.Fatal(err)
			}

			if sec == nil {
				log.Fatal("unable to login to get token")
			}

			vaultClient.SetToken(sec.Auth.ClientToken)
			L.Info("retrieved token from vault", "accessor", sec.Auth.Accessor)

			go func() {
				tic := time.NewTicker(time.Hour)
				for {
					<-tic.C
					_, err := vaultClient.Auth().Token().RenewSelf(86400)
					if err != nil {
						log.Printf("unable to renew Vault token: %v", err)
					}
				}
			}()
		}
	}

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		log.Fatal("no DATABASE_URL provided")
	}

	db, err := gorm.Open("postgres", url)
	if err != nil {
		log.Fatal(err)
	}

	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		log.Fatal("S3_BUCKET not set")
	}

	useAWS := os.Getenv("USE_AWS") != "0"

	var sess *session.Session = nil
	if useAWS {
		sess, err = session.NewSession(&aws.Config{})
		if err != nil {
			log.Fatalf("unable to initialize AWS: %v", err)
		}
	}


	domain := os.Getenv("HUB_DOMAIN")
	if domain == "" {
		log.Fatal("missing HUB_DOMAIN")
	}

	useTLSManager := os.Getenv("USE_TLS_MANAGER") != "0"

	var tlsmgr *tlsmanage.Manager = nil
	if useTLSManager {
		staging := os.Getenv("LETSENCRYPT_STAGING") != ""
		tlsmgr, err = tlsmanage.NewManager(tlsmanage.ManagerConfig{
			L:           L,
			Domain:      domain,
			VaultClient: vaultClient,
			Staging:     staging,
		})
		if err != nil {
			log.Fatal(err)
		}
	}

	if useAWS && useTLSManager {
		zoneId := os.Getenv("ZONE_ID")
		if zoneId == "" {
			log.Fatal("missing ZONE_ID")
		}

		err = tlsmgr.SetupRoute53(sess, zoneId)
		if err != nil {
			log.Fatal(err)
		}
	}

	regTok := os.Getenv("REGISTER_TOKEN")
	if regTok == "" {
		log.Fatal("missing REGISTER_TOKEN")
	}

	opsTok := os.Getenv("OPS_TOKEN")
	if opsTok == "" {
		log.Fatal("missing OPS_TOKEN")
	}

	asnDB := os.Getenv("ASN_DB_PATH")

	hubAccess := os.Getenv("HUB_ACCESS_KEY")
	hubSecret := os.Getenv("HUB_SECRET_KEY")
	hubTag := os.Getenv("HUB_IMAGE_TAG")

	port := os.Getenv("PORT")
	if port == "" {
		port = "24402"
	}

	go StartHealthz(L)

	ctx := hclog.WithContext(context.Background(), L)

	var cert []byte = nil
	var key []byte = nil

	if tlsmgr != nil {
		cert, key, err = tlsmgr.HubMaterial(ctx)
		if err != nil {
			log.Fatal(err)
		}
	}

	lm, err := control.NewConsulLockManager(ctx)
	if err != nil {
		log.Fatal(err)
	}

	s, err := control.NewServer(control.ServerConfig{
		Logger: L,
		DB:     db,

		RegisterToken: regTok,
		OpsToken:      opsTok,

		VaultClient: vaultClient,
		VaultPath:   "hzn-k1",
		KeyId:       "k1",

		AwsSession: sess,
		Bucket:     bucket,

		ASNDB: asnDB,

		HubAccessKey: hubAccess,
		HubSecretKey: hubSecret,
		HubImageTag:  hubTag,
		LockManager:  lm,
	})
	if err != nil {
		log.Fatal(err)
	}

	// Setup cleanup activities
	lc := &control.LogCleaner{DB: config.DB()}
	workq.RegisterHandler("cleanup-activity-log", lc.CleanupActivityLog)
	workq.RegisterPeriodicJob("cleanup-activity-log", "default", "cleanup-activity-log", nil, time.Hour)

	hubDomain := domain
	if strings.HasPrefix(hubDomain, "*.") {
		hubDomain = hubDomain[2:]
	}

	var tlsCert *tls.Certificate = nil

	if tlsmgr != nil {
		s.SetHubTLS(cert, key, hubDomain)

		// So that when they are refreshed by the background job, we eventually pick
		// them up. Hubs are also refreshing their config on an hourly basis so they'll
		// end up picking up the new TLS material that way too.
		go periodic.Run(ctx, time.Hour, func() {
			cert, key, err := tlsmgr.RefreshFromVault()
			if err != nil {
				L.Error("error refreshing hub certs from vault")
			} else {
				s.SetHubTLS(cert, key, hubDomain)
			}
		})

		cert, err := tlsmgr.Certificate()
		if err != nil {
			log.Fatal(err)
		}
		tlsCert = &cert
	}

	gs := grpc.NewServer()
	pb.RegisterControlServicesServer(gs, s)
	pb.RegisterControlManagementServer(gs, s)
	pb.RegisterFlowTopReporterServer(gs, s)
	reflection.Register(gs)

	var lcfg *tls.Config = nil
	if tlsmgr != nil && tlsCert != nil {
		lcfg = &tls.Config{}
		lcfg.Certificates = []tls.Certificate{*tlsCert}
	} else {
		// HTTP/2 requires a TLS certificate, let's generate a self-signed one
		cert, privateKey := snakeOilCert("127.0.0.1:24402")
		lcfg = &tls.Config{}
		tlsCert := tls.Certificate{
			Certificate: [][]byte{cert},
			PrivateKey: privateKey,
		}
		lcfg.Certificates = []tls.Certificate{tlsCert}
	}

	hs := &http.Server{
		TLSConfig:   lcfg,
		Addr:        ":" + port,
		IdleTimeout: 2 * time.Minute,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 &&
				strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
				gs.ServeHTTP(w, r)
			} else {
				s.ServeHTTP(w, r)
			}
		}),
		ErrorLog: L.StandardLogger(&hclog.StandardLoggerOptions{
			InferLevels: true,
		}),
	}

	tlsmgr.RegisterRenewHandler(L, workq.GlobalRegistry)

	L.Info("starting background worker")

	workq.GlobalRegistry.PrintHandlers(L)

	wl := L.Named("workq")

	worker := workq.NewWorker(wl, db, []string{"default"})
	go func() {
		err := worker.Run(ctx, workq.RunConfig{
			ConnInfo: url,
		})
		if err != nil {
			if err != context.Canceled {
				wl.Debug("workq errored out in run", "error", err)
			}
		}
	}()

	err = hs.ListenAndServeTLS("", "")
	if err != nil {
		log.Fatal(err)
	}

	return 0
}

func snakeOilCert(commonName string) ([]byte, *rsa.PrivateKey) {
	privateSnakeOil, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		log.Fatalf("unable to create snakeoil ed25519 key pair: %v", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)

	if err != nil {
		log.Fatalf("failed to generate serial number: %v", err)
	}

	subj := pkix.Name{
		Country: []string{"CH"},
		Organization: []string{
			"SnakeOil",
		},
		Names:      nil,
		ExtraNames: nil,
		CommonName: commonName,
	}

	notBefore := time.Now()
	// 5 years, hopefully the instance won't stay up longer than this
	notAfter := notBefore.Add(5 * 365 * 24 * time.Hour)

	template := x509.Certificate{
		Subject:      subj,
		SerialNumber: serialNumber,
		NotBefore:    time.Now(),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}

	cert, err := x509.CreateCertificate(rand.Reader, &template, &template, privateSnakeOil.Public(), privateSnakeOil)
	if err != nil {
		log.Fatalf("unable to create x509 certificate: %v", err)
	}
	return cert, privateSnakeOil
}