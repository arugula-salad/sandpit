// sandpit-kube is the spike of sandpit on Kubernetes: the E2B API (the same
// front end sandpitd serves) on the kube engine, whose sandboxes are
// agent-sandbox Sandboxes rather than Firecracker VMs, or (--engine
// substrate) on the substrate engine, whose sandboxes are Agent Substrate
// actors that suspend and resume warm. The official E2B SDKs
// work against it unmodified. docs/plans/kubernetes-backend.md is the plan
// that would fold this into sandpitd (--backend=kube) for every API.
//
//	sandpit-kube --kubeconfig ~/.kube/config --context kind-sandpit \
//	  --namespace sandpit --image sandpit-kube-e2b:dev --dial port-forward
//
// It authenticates E2B's X-API-Key against one token: SANDPIT_TOKEN, or the
// token file in the data directory, written on first start.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/arugula-salad/sandpit/frontend/e2b"
	"github.com/arugula-salad/sandpit/internal/store"
	"github.com/arugula-salad/sandpit/kube"
	"github.com/arugula-salad/sandpit/substrate"
)

func main() {
	home, _ := os.UserHomeDir()
	var (
		listen      = flag.String("e2b-listen", "127.0.0.1:7901", "the E2B API's address")
		domain      = flag.String("e2b-domain", "", "the E2B domain sandboxes are reported under (<port>-<id>.<domain> reaches this listener); default the listen address under e2b.localhost")
		dataDir     = flag.String("data-dir", filepath.Join(home, ".local", "share", "sandpit-kube"), "where the records and the token are kept")
		kubeconfig  = flag.String("kubeconfig", "", "kubeconfig; default $KUBECONFIG, ~/.kube/config, or in-cluster")
		kubeContext = flag.String("context", "", "kubeconfig context; default the current one")
		namespace   = flag.String("namespace", "sandpit", "namespace the Sandboxes are made in")
		image       = flag.String("image", "", "guest image: a userland with sandpit-agent as its entrypoint (images/kube)")
		pull        = flag.String("image-pull-policy", "", "Always, IfNotPresent or Never; default Kubernetes' for the tag")
		runtimeCls  = flag.String("runtime-class", "", "the pods' RuntimeClass (gvisor, kata-qemu, ...); default the cluster's, which is no sandbox at all")
		dial        = flag.String("dial", kube.DialPodIP, "how to reach a pod: pod-ip (in the cluster) or port-forward (through the API server)")
		persist     = flag.String("persist", "/home/user", "directory on a per-sandbox volume, which survives a pause; empty for none")
		diskSize    = flag.String("disk-size", "10Gi", "size of that volume")
		storageCls  = flag.String("storage-class", "", "that volume's StorageClass; default the cluster's")
		cpus        = flag.Int("cpus", 2, "CPUs per sandbox (request and limit)")
		mem         = flag.Int("mem", 512, "memory per sandbox, MiB (request and limit)")
		maxTimeout  = flag.Duration("e2b-max-timeout", 24*time.Hour, "longest timeout a sandbox may be given")

		engineName  = flag.String("engine", "kube", "kube (agent-sandbox Sandboxes) or substrate (Agent Substrate actors)")
		ateAPI      = flag.String("ate-api", "127.0.0.1:8443", "substrate: ate-api-server's gRPC address")
		ateCA       = flag.String("ate-ca", "", "substrate: CA file for ate-api's certificate")
		ateToken    = flag.String("ate-token", "", "substrate: file holding a bearer token ate-api accepts")
		ateRouter   = flag.String("ate-router", "127.0.0.1:8081", "substrate: atenet-router's CONNECT listener")
		atespace    = flag.String("atespace", "sandpit", "substrate: atespace the actors are made in")
		ateTemplate = flag.String("template", "e2b", "substrate: the ActorTemplate actors are made from (images/substrate); with --template-file, the base of its name")
		ateTmplFile = flag.String("template-file", "", "substrate: an ActorTemplate (YAML) to make at startup, with the atespace, as <template>-<hash of the file>")
		ateServer   = flag.String("ate-api-server-name", "api.ate-system.svc", "substrate: the name ate-api's certificate is for")
		idleSuspend = flag.Duration("idle-suspend", 0, "substrate: suspend a sandbox idle this long, warm; the next request resumes it (0: never)")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fatal := func(msg string, err error) {
		log.Error(msg, "err", err)
		os.Exit(1)
	}
	if *image == "" && *engineName == "kube" {
		fatal("flags", errors.New("--image is required"))
	}
	if *domain == "" {
		_, port, _ := strings.Cut(*listen, ":")
		*domain = "e2b.localhost:" + port
	}

	st, err := store.Open(*dataDir)
	if err != nil {
		fatal("store", err)
	}
	token, err := loadToken(*dataDir)
	if err != nil {
		fatal("token", err)
	}

	var eng interface {
		e2b.Engine
		Shutdown()
	}
	switch *engineName {
	case "kube":
		// Only this engine talks to the Kubernetes API; substrate's reaches ate-api alone.
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		rules.ExplicitPath = *kubeconfig
		cfg, cerr := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: *kubeContext}).ClientConfig()
		if cerr != nil {
			fatal("kubeconfig", cerr)
		}
		eng, err = kube.New(kube.Options{Namespace: *namespace, Image: *image, ImagePullPolicy: corev1.PullPolicy(*pull),
			RuntimeClass: *runtimeCls, Dial: *dial, PersistPath: *persist, DiskSize: *diskSize, StorageClass: *storageCls},
			cfg, st, log)
	case "substrate":
		eng, err = substrate.New(substrate.Options{API: *ateAPI, APIServerName: *ateServer, CAFile: *ateCA, TokenFile: *ateToken,
			Router: *ateRouter, Atespace: *atespace, Template: *ateTemplate, TemplateFile: *ateTmplFile, IdleSuspend: *idleSuspend}, st, log)
	default:
		err = fmt.Errorf("unknown --engine %q", *engineName)
	}
	if err != nil {
		fatal("engine", err)
	}
	defer eng.Shutdown()
	fe := e2b.New(e2b.Options{Domain: *domain, MaxTimeout: *maxTimeout, CPUs: *cpus, MemMiB: *mem,
		DefaultCPUs: *cpus, DefaultMemMiB: *mem,
		CheckKey: func(key string) (admin, ok bool) {
			ok = subtle.ConstantTimeCompare([]byte(key), []byte(token)) == 1
			return ok, ok
		}}, st, eng, log)

	srv := &http.Server{Addr: *listen, Handler: fe.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		srv.Close()
	}()
	log.Info("serving the E2B API on Kubernetes", "engine", *engineName, "listen", *listen, "domain", *domain, "namespace", *namespace,
		"image", *image, "runtime_class", *runtimeCls, "dial", *dial)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal("serve", err)
	}
}

// loadToken is SANDPIT_TOKEN, else <data>/token, made on first start.
func loadToken(dir string) (string, error) {
	if t := os.Getenv("SANDPIT_TOKEN"); t != "" {
		return t, nil
	}
	path := filepath.Join(dir, "token")
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 24)
	rand.Read(b)
	t := hex.EncodeToString(b)
	return t, os.WriteFile(path, []byte(t+"\n"), 0o600)
}
