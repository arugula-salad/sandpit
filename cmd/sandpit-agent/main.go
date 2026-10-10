// sandpit-agent is the in-guest runtime. As PID 1 it sets up the system and
// supervises a copy of itself running `serve`, which hosts the agent API on
// vsock. Splitting the two keeps PID 1's wait4(-1) orphan reaping from
// stealing exit statuses that os/exec is waiting on in the server. As a
// container's entrypoint (`sandpit-agent serve ...` as PID 1, in a Kubernetes
// pod) it does the same supervising, on TCP, and none of the system setup.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	"github.com/arugula-salad/sandpit/internal/agent"
)

// AgentPort is the vsock port sandpitd dials.
const AgentPort = 1024

// hostAPIPort is the vsock port sandpitd answers on for this sprite (guestAPIPort there).
const hostAPIPort = 1025

func main() {
	log.SetFlags(0)
	log.SetPrefix("sandpit-agent: ")
	if os.Getpid() == 1 {
		if len(os.Args) > 1 && os.Args[1] == "serve" {
			runContainerInit() // a container's entrypoint, not a VM's init
			return
		}
		runInit()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serve(os.Args[2:])
		return
	}
	fmt.Fprintln(os.Stderr, "usage: sandpit-agent serve [--listen vsock|tcp:ADDR|unix:PATH]")
	os.Exit(2)
}

func dialHost(context.Context) (net.Conn, error) { return vsock.Dial(vsock.Host, hostAPIPort, nil) }

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "vsock", "vsock, tcp:HOST:PORT or unix:PATH (the latter two are for host-side testing)")
	stateDir := fs.String("state-dir", "/.sprite", "where service definitions and logs live (on the sprite's disk)")
	runDir := fs.String("run-dir", "/run/sprite-services", "pid files; must not survive a reboot")
	systemDir := fs.String("system-services-dir", "/etc/sandpit/services.d", "the image's own daemons, started at boot (vsock only); /etc/wisp/services.d is read when it is absent, for disks built before the rename")
	system := fs.Bool("system-services", false, "start the image's own daemons on a tcp listener too: the agent is a sandbox's entrypoint (a Kubernetes pod), not a test on the host")
	fs.Parse(args)
	*system = *system || *listen == "vsock"
	// A pod's agent is reachable from the pod network, so it asks every stream
	// for the token it was started with (agent.TokenListener).
	token := os.Getenv("SANDPIT_AGENT_TOKEN")
	os.Unsetenv("SANDPIT_AGENT_TOKEN") // not the sandbox's to read in its sessions' environment

	var ln net.Listener
	var err error
	switch {
	case *listen == "vsock":
		ln, err = vsock.Listen(AgentPort, nil)
	case strings.HasPrefix(*listen, "tcp:"):
		ln, err = net.Listen("tcp", strings.TrimPrefix(*listen, "tcp:"))
	case strings.HasPrefix(*listen, "unix:"):
		ln, err = net.Listen("unix", strings.TrimPrefix(*listen, "unix:"))
	default:
		log.Fatalf("bad --listen %q", *listen)
	}
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	if token != "" {
		ln = agent.TokenListener(ln, token)
	} else if *system && *listen != "vsock" {
		log.Fatal("SANDPIT_AGENT_TOKEN is required with --system-services on a network listener")
	}

	// Before the supervisor exists: it starts services, and they must be confined too.
	boot := cmdline()
	memLimit, _ := strconv.Atoi(boot["memlimit"])
	agent.InitPolicy(agent.Policy{Profile: boot["profile"], NoNewPrivs: boot["nnp"] == "1", MemoryLimitMB: memLimit},
		"/run/sprite-policy.json")

	// The image's own daemons (E2B's envd, say), not the user's: they are
	// started here and are nowhere in the services API. Only in a real guest (a
	// VM, or a pod with --system-services): on the host these paths would name
	// the host's files.
	if *system {
		dir := *systemDir
		if _, err := os.Stat(dir); os.IsNotExist(err) && dir == "/etc/sandpit/services.d" {
			dir = "/etc/wisp/services.d" // a disk built by wisp
		}
		agent.NewSystemSupervisor(dir, "/var/log/sandpit/services", "/run/sandpit-system-services")
	}

	// Service starts and crashes go to sandpitd's event stream, which only exists
	// over vsock, and so does the sprite's environment the services start with.
	var report func(agent.ServiceReport)
	var spriteEnv []string
	if *listen == "vsock" {
		report = agent.NewReporter(dialHost).Report
		if spriteEnv, err = agent.FetchSpriteEnv(dialHost, 5*time.Second); err != nil {
			log.Printf("services start without the sprite's environment: %v", err)
		}
	}
	srv := &agent.Server{
		Sessions: agent.NewManager(),
		StateDir: *stateDir,
		Services: agent.NewSpriteSupervisor(*stateDir, *runDir, report, spriteEnv),
		Poweroff: func() {
			unix.Sync()
			// With reboot=k on the kernel command line this resets via the
			// keyboard controller, which makes Firecracker exit cleanly.
			if err := unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART); err != nil {
				log.Printf("reboot: %v", err)
			}
		},
	}
	// The in-guest API needs the host channel, which only exists over vsock.
	if *listen == "vsock" {
		sock := filepath.Join(*stateDir, "api.sock")
		if gl, err := agent.ListenGuestAPI(sock); err != nil {
			log.Printf("%s: %v", sock, err)
		} else {
			gs := &http.Server{Handler: srv.GuestAPI(dialHost), ReadHeaderTimeout: 10 * time.Second}
			go func() { log.Fatal(gs.Serve(gl)) }()
		}
	}
	log.Printf("serving on %s", *listen)
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(hs.Serve(ln))
}
