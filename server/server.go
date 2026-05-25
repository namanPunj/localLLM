package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"assistant/chat"
	"assistant/files"
	"assistant/sessions"
	"assistant/terminal"
)

type Server struct {
	pipeline *chat.Pipeline
	sessions *sessions.Store
	files    *files.Store
	scraper  URLScraper
	settings *SettingsStore
	memory   *PersonalMemory
	ollama   *chat.Ollama
	routines *RoutineStore
	push     *PushStore

	queue    *GlobalQueue
	workerMu sync.Once // ensures runWorker starts exactly once
}

func New(p *chat.Pipeline, s *sessions.Store, f *files.Store, scraper URLScraper, settings *SettingsStore, memory *PersonalMemory, ollama *chat.Ollama, routines *RoutineStore, push *PushStore) *Server {
	srv := &Server{
		pipeline: p,
		sessions: s,
		files:    f,
		scraper:  scraper,
		settings: settings,
		memory:   memory,
		ollama:   ollama,
		routines: routines,
		push:     push,
		queue:    NewGlobalQueue(),
	}
	routines.push = push
	go srv.runWorker()
	routines.RunScheduler(srv.queue, p, settings)
	return srv
}

// runWorker is the single goroutine that drains the global queue sequentially.
// It runs for the lifetime of the server.
func (s *Server) runWorker() {
	for {
		entry := s.queue.next() // blocks until work arrives

		sid := entry.req.SessionID
		sr := s.queue.GetOrCreateReplay(sid)
		sr.clear()

		ctx, cancel := context.WithCancel(context.Background())
		s.queue.setCancel(cancel)

		var tr *terminal.Translator
		if entry.termMode {
			tr = terminal.New()
		}

		emit := func(ev chat.StreamEvent) error {
			if entry.termMode && ev.Type == "token" {
				ev.Content = tr.Push(ev.Content)
				if ev.Content == "" {
					return nil
				}
			}
			sr.broadcast(ev, entry.events)
			return nil
		}

		if err := s.pipeline.Run(ctx, entry.req, emit); err != nil {
			sr.broadcast(chat.StreamEvent{Type: "error", Error: err.Error()}, entry.events)
		}
		if entry.termMode {
			if tail := tr.Flush(); tail != "" {
				sr.broadcast(chat.StreamEvent{Type: "token", Content: tail}, entry.events)
			}
		}

		// Signal the originating HTTP handler that this entry is finished.
		close(entry.events)

		s.queue.clearCurrent()
		cancel()
	}
}

func (s *Server) Run(addr string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/chat/web", s.handleChat(false))
	mux.HandleFunc("/chat/terminal", s.handleChat(true))
	mux.HandleFunc("/chat/cancel", s.handleChatCancel)
	mux.HandleFunc("/chat/stream/", s.handleChatStream) // GET — reconnect to live stream
	mux.HandleFunc("/chat/queue/", s.handleQueueStatus)  // GET — active+queued count

	mux.HandleFunc("/upload", s.handleUpload)
	mux.HandleFunc("/files/edit", s.handleFileEdit)
	mux.HandleFunc("/files/raw/", s.handleFileRaw)
	mux.HandleFunc("/scrape", s.handleScrape)
	mux.HandleFunc("/settings", s.handleSettings)
	mux.HandleFunc("/memory", s.handleMemory)
	mux.HandleFunc("/routines", s.handleRoutines)
	mux.HandleFunc("/routines/", s.handleRoutineByID)
	mux.HandleFunc("/push/vapid-key", s.handlePushVAPIDKey)
	mux.HandleFunc("/push/subscribe", s.handlePushSubscribe)
	mux.HandleFunc("/data", s.handleDeleteAllData)

	mux.HandleFunc("/sessions", s.handleSessionsList)
	mux.HandleFunc("/sessions/", s.handleSessionByID)
	mux.HandleFunc("/folders", s.handleFolders)
	mux.HandleFunc("/folders/", s.handleFolders)
	mux.HandleFunc("/model/wake", s.handleModelWake)
	mux.HandleFunc("/model/swap", s.handleModelSwap)
	mux.HandleFunc("/tts", s.handleTTS)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	fs := http.FileServer(http.Dir("./web"))
	mux.Handle("/", fs)

	srv := &http.Server{
		Addr:              addr,
		Handler:           cors(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	certFile, keyFile := ensureTLSCert("./data/certs")
	if certFile != "" {
		log.Printf("[tls] serving HTTPS on %s", addr)
		return srv.ListenAndServeTLS(certFile, keyFile)
	}
	return srv.ListenAndServe()
}

// getLANIPs returns all non-loopback IPv4 addresses on this machine.
func getLANIPs() []net.IP {
	var ips []net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			ips = append(ips, ipnet.IP)
		}
	}
	return ips
}

// ensureTLSCert generates a self-signed cert in dir that covers localhost +
// all current LAN IPs. If the IPs have changed since last generation, the
// cert is regenerated automatically. Returns ("","") if generation fails.
func ensureTLSCert(dir string) (certPath, keyPath string) {
	certPath = dir + "/cert.pem"
	keyPath = dir + "/key.pem"
	ipsPath := dir + "/ips.txt"

	lanIPs := getLANIPs()
	if len(lanIPs) == 0 {
		log.Printf("[tls] no LAN IPs found, skipping TLS")
		return "", ""
	}

	// Build current IP string for comparison.
	var ipStrs []string
	for _, ip := range lanIPs {
		ipStrs = append(ipStrs, ip.String())
	}
	currentIPs := strings.Join(ipStrs, ",")

	// Check if cert exists and IPs match.
	if _, err := os.Stat(certPath); err == nil {
		if saved, err := os.ReadFile(ipsPath); err == nil && strings.TrimSpace(string(saved)) == currentIPs {
			log.Printf("[tls] cert valid for %s", currentIPs)
			return certPath, keyPath
		}
		log.Printf("[tls] LAN IP changed, regenerating cert")
	}

	// Generate new self-signed certificate.
	_ = os.MkdirAll(dir, 0o755)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Printf("[tls] keygen failed: %v", err)
		return "", ""
	}

	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{CommonName: "Assistant"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},

		IPAddresses: append(lanIPs, net.IPv4(127, 0, 0, 1)),
		DNSNames:    []string{"localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		log.Printf("[tls] cert creation failed: %v", err)
		return "", ""
	}

	// Write cert PEM.
	certOut, _ := os.Create(certPath)
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certOut.Close()

	// Write key PEM.
	keyBytes, _ := x509.MarshalECPrivateKey(key)
	keyOut, _ := os.Create(keyPath)
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	keyOut.Close()

	// Save IPs for next startup comparison.
	_ = os.WriteFile(ipsPath, []byte(currentIPs), 0o644)

	log.Printf("[tls] generated cert for IPs: %s", currentIPs)
	return certPath, keyPath
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}
