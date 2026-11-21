// Command mailbridge serves the Mac's Apple Mail and Messages databases over a
// read-only, authenticated HTTPS API on the LAN. Data stays on the Mac;
// consumers pull it. See ../../README.md.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/geekychris/apple_mail_dump/bridge/internal/maildb"
	"github.com/geekychris/apple_mail_dump/bridge/internal/secrets"
	"github.com/geekychris/apple_mail_dump/bridge/internal/server"
)

var version = "dev"

func main() {
	server.Version = version
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "init":
		initCmd(os.Args[2:])
	case "fingerprint":
		fingerprintCmd(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: mailbridge <command> [flags]

commands:
  serve        run the HTTPS API (what the launchd agent runs)
  init         create the token and TLS certificate if missing; print the fingerprint
  fingerprint  print the certificate's SHA-256 fingerprint
  version      print the version

The token is never printed; read it from <dir>/token (mode 0600).`)
}

func defaultDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "mailbridge")
}

func certHosts(bindIP string) []string {
	h, _ := os.Hostname()
	hosts := []string{"localhost", "127.0.0.1"}
	if h != "" {
		short := strings.TrimSuffix(h, ".local")
		hosts = append(hosts, short, short+".local")
	}
	if bindIP != "" {
		hosts = append(hosts, bindIP)
	}
	return hosts
}

func initCmd(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "state directory")
	fs.Parse(args)
	f := secrets.Files{Dir: *dir}
	ip, _ := pickBindIP("auto", defaultAllow(false))
	created, err := f.Ensure(certHosts(ip))
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	_, fp, err := f.LoadCert()
	if err != nil {
		log.Fatalf("load cert: %v", err)
	}
	if len(created) > 0 {
		fmt.Printf("created: %s\n", strings.Join(created, ", "))
	}
	fmt.Printf("state dir:   %s\ntoken file:  %s (mode 0600; not printed)\nfingerprint: %s\n", *dir, f.Token(), fp)
}

func fingerprintCmd(args []string) {
	fs := flag.NewFlagSet("fingerprint", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "state directory")
	fs.Parse(args)
	_, fp, err := secrets.Files{Dir: *dir}.LoadCert()
	if err != nil {
		log.Fatalf("load cert: %v", err)
	}
	fmt.Println(fp)
}

func defaultAllow(tailscale bool) []*net.IPNet {
	cidrs := []string{"192.168.1.0/24"}
	if tailscale {
		cidrs = append(cidrs, "100.64.0.0/10")
	}
	nets, _ := parseCIDRs(strings.Join(cidrs, ","))
	return nets
}

func parseCIDRs(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, c := range strings.Split(s, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("bad CIDR %q: %w", c, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// pickBindIP resolves "auto" to the first IPv4 address on an up, non-loopback
// interface that lies inside one of the allowed networks. Anything else must
// be a literal IP. The bridge never binds 0.0.0.0.
func pickBindIP(bind string, allow []*net.IPNet) (string, error) {
	if bind != "auto" {
		ip := net.ParseIP(bind)
		if ip == nil || ip.IsUnspecified() {
			return "", fmt.Errorf("--bind must be 'auto' or a specific IP address, not %q", bind)
		}
		return ip.String(), nil
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			for _, n := range allow {
				if n.Contains(ipn.IP) {
					return ipn.IP.String(), nil
				}
			}
		}
	}
	return "", errors.New("no interface address inside the allowed networks (is the LAN up?)")
}

func hasLocalIP(ip string) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true // can't tell; don't flap
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.String() == ip {
			return true
		}
	}
	return false
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	home, _ := os.UserHomeDir()
	dir := fs.String("dir", defaultDir(), "state directory (token, certificate, snapshots)")
	bind := fs.String("bind", "auto", "address to listen on: 'auto' (the LAN address inside --allow) or an IP")
	port := fs.Int("port", 18443, "TCP port")
	allowS := fs.String("allow", "192.168.1.0/24", "comma-separated source networks allowed to connect")
	tailscale := fs.Bool("allow-tailscale", false, "also allow 100.64.0.0/10 (Tailscale)")
	mailRoot := fs.String("mail-root", filepath.Join(home, "Library", "Mail"), "Apple Mail data directory")
	chatPath := fs.String("chat-db", filepath.Join(home, "Library", "Messages", "chat.db"), "Messages chat.db")
	noMail := fs.Bool("no-mail", false, "disable the mail endpoints")
	noMessages := fs.Bool("no-messages", false, "disable the Messages endpoints")
	maxAge := fs.Duration("snapshot-age", time.Minute, "how stale a database snapshot may get before it is re-checked")
	rate := fs.Float64("rate", 20, "requests per second per source address")
	burst := fs.Float64("burst", 60, "burst size per source address")
	fs.Parse(args)

	logger := log.New(os.Stderr, "mailbridge ", log.LstdFlags|log.LUTC)
	allow, err := parseCIDRs(*allowS)
	if err != nil {
		logger.Fatal(err)
	}
	if *tailscale {
		allow = append(allow, defaultAllow(true)[1])
	}
	ip, err := pickBindIP(*bind, allow)
	if err != nil {
		logger.Fatalf("bind: %v", err)
	}
	f := secrets.Files{Dir: *dir}
	if created, err := f.Ensure(certHosts(ip)); err != nil {
		logger.Fatalf("secrets: %v", err)
	} else if len(created) > 0 {
		logger.Printf("created %s in %s", strings.Join(created, " and "), *dir)
	}
	token, err := f.LoadToken()
	if err != nil {
		logger.Fatalf("token: %v", err)
	}
	cert, fp, err := f.LoadCert()
	if err != nil {
		logger.Fatalf("certificate: %v", err)
	}
	snapDir := filepath.Join(*dir, "snapshots")
	_ = os.RemoveAll(snapDir) // stale copies from a previous run
	if err := os.MkdirAll(snapDir, 0o700); err != nil {
		logger.Fatal(err)
	}

	srv := &server.Server{Token: token, Allow: allow, Log: logger, Limiter: server.NewLimiter(*rate, *burst)}
	if !*noMail {
		srv.Mail = &server.MailSource{Root: *mailRoot, SnapshotDir: filepath.Join(snapDir, "mail"), MaxAge: *maxAge,
			Emlx: maildb.NewEmlxIndex(*mailRoot, 5*time.Minute)}
	}
	if !*noMessages {
		srv.Chat = &server.ChatSource{Path: *chatPath, SnapshotDir: filepath.Join(snapDir, "messages"), MaxAge: *maxAge}
	}

	addr := net.JoinHostPort(ip, strconv.Itoa(*port))
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          logger,
	}
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		logger.Fatalf("listen %s: %v", addr, err)
	}
	nets := make([]string, len(allow))
	for i, n := range allow {
		nets[i] = n.String()
	}
	logger.Printf("version=%s listening https://%s allow=%s mail=%v messages=%v fingerprint=%s",
		version, addr, strings.Join(nets, ","), !*noMail, !*noMessages, fp)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// If DHCP moves the LAN address, exit so launchd restarts us on the new one.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !hasLocalIP(ip) {
					logger.Printf("bind address %s is gone; exiting so launchd restarts on the new address", ip)
					stop()
					os.Exit(3)
				}
			}
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	if err := hs.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("serve: %v", err)
	}
}
