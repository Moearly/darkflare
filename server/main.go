// Copyright (c) Barrett Lyon
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Session struct {
	conn       net.Conn
	lastActive time.Time
	buffer     []byte
	mu         sync.Mutex
}

type Server struct {
	sessions     sync.Map
	sessionMutex sync.Mutex
	destHost     string
	destPort     string
	debug        bool
	appCommand   string
	isAppMode    bool
	allowDirect  bool
}

func NewServer(destHost, destPort string, appCommand string, debug bool, allowDirect bool) *Server {
	s := &Server{
		destHost:    destHost,
		destPort:    destPort,
		debug:       debug,
		appCommand:  appCommand,
		isAppMode:   appCommand != "",
		allowDirect: allowDirect,
	}

	if s.debug {
		log.Printf("Server configuration:")
		log.Printf("  Allow Direct: %v", allowDirect)
		log.Printf("  Debug Mode: %v", debug)
		log.Printf("  App Mode: %v", s.isAppMode)
	}

	go s.cleanupSessions()
	return s
}

func (s *Server) cleanupSessions() {
	for {
		time.Sleep(time.Minute)
		now := time.Now()
		s.sessions.Range(func(key, value interface{}) bool {
			session := value.(*Session)
			session.mu.Lock()
			if now.Sub(session.lastActive) > 5*time.Minute {
				session.conn.Close()
				s.sessions.Delete(key)
			}
			session.mu.Unlock()
			return true
		})
	}
}

func (s *Server) handleApplication(w http.ResponseWriter, r *http.Request) {
	if s.debug {
		log.Printf("Handling application request from %s", r.Header.Get("Cf-Connecting-Ip"))
	}

	parts := strings.Fields(s.appCommand)
	if len(parts) == 0 {
		http.Error(w, "Invalid application command", http.StatusInternalServerError)
		return
	}

	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Env = os.Environ()

	if s.debug {
		log.Printf("Launching application: %s", s.appCommand)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to create stdout pipe: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("Failed to create stderr pipe: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start application: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Handle stdout in a goroutine
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if s.debug {
				log.Printf("Application stdout: %s", scanner.Text())
			}
		}
		if err := scanner.Err(); err != nil && s.debug {
			log.Printf("Error reading stdout: %v", err)
		}
	}()

	// Handle stderr in a goroutine
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if s.debug {
				log.Printf("Application stderr: %s", scanner.Text())
			}
		}
		if err := scanner.Err(); err != nil && s.debug {
			log.Printf("Error reading stderr: %v", err)
		}
	}()

	if err := cmd.Wait(); err != nil {
		if s.debug {
			log.Printf("Application exited with error: %v", err)
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (s *Server) handleRequest(w http.ResponseWriter, r *http.Request) {
	// Get client IP from various possible sources
	clientIP := r.Header.Get("Cf-Connecting-Ip")
	if clientIP == "" {
		// Try X-Real-IP
		clientIP = r.Header.Get("X-Real-IP")
		if clientIP == "" {
			// Try X-Forwarded-For
			clientIP = r.Header.Get("X-Forwarded-For")
			if clientIP == "" {
				// Finally, use RemoteAddr
				clientIP, _, _ = net.SplitHostPort(r.RemoteAddr)
			}
		}
	}

	if s.debug {
		log.Printf("Request: %s %s from %s",
			r.Method,
			r.URL.Path,
			clientIP,
		)
		log.Printf("Headers: %+v", r.Header)
	}

	// Verify Cloudflare connection
	if clientIP == "" && !s.allowDirect {
		http.Error(w, "Direct access not allowed", http.StatusForbidden)
		return
	}

	// Set Apache-like headers
	w.Header().Set("Server", "Apache/2.4.41 (Ubuntu)")
	w.Header().Set("X-Powered-By", "PHP/7.4.33")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("X-XSS-Protection", "1; mode=block")

	// Cache control headers
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Content-Type", "application/octet-stream")

	// Get the encoded destination from headers
	encodedDest := r.Header.Get("X-Requested-With")
	if encodedDest == "" {
		if s.debug {
			log.Printf("[DEBUG] Missing X-Requested-With header")
		}
		http.Error(w, "Missing destination", http.StatusBadRequest)
		return
	}

	// Decode the destination
	destBytes, err := base64.StdEncoding.DecodeString(encodedDest)
	if err != nil {
		if s.debug {
			log.Printf("[DEBUG] Failed to decode X-Requested-With: %v", err)
		}
		http.Error(w, "Invalid destination encoding", http.StatusBadRequest)
		return
	}

	destination := string(destBytes)
	if s.debug {
		log.Printf("[DEBUG] Decoded destination: %s", destination)
	}

	// Validate the destination
	if !isValidDestination(destination) {
		if s.debug {
			log.Printf("[DEBUG] Invalid destination format: %s", destination)
		}
		http.Error(w, "Invalid destination", http.StatusForbidden)
		return
	}

	// Use the decoded destination for the connection
	host, port, err := net.SplitHostPort(destination)
	if err != nil {
		if s.debug {
			log.Printf("[DEBUG] Failed to split host:port: %v", err)
		}
		http.Error(w, "Invalid destination format", http.StatusBadRequest)
		return
	}

	if s.debug {
		log.Printf("[DEBUG] Connecting to %s:%s", host, port)
	}

	// Try to get session ID from various possible headers
	sessionID := r.Header.Get("X-For")
	if sessionID == "" {
		// Try Cloudflare-specific headers
		sessionID = r.Header.Get("Cf-Ray")
		if sessionID == "" {
			// Could also try other headers or generate a session ID based on IP
			sessionID = r.Header.Get("Cf-Connecting-Ip")
		}
	}

	if sessionID == "" {
		if s.debug {
			log.Printf("Error: Missing session ID from %s", r.Header.Get("Cf-Connecting-Ip"))
		}
		http.Error(w, "Missing session ID", http.StatusBadRequest)
		return
	}

	var session *Session
	sessionInterface, exists := s.sessions.Load(sessionID)
	if !exists {
		if s.debug {
			log.Printf("[DEBUG] No existing session found for %s, creating new session", sessionID[:8])
		}
		conn, err := net.Dial("tcp", fmt.Sprintf("%s:%s", host, port))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		session = &Session{
			conn:       conn,
			lastActive: time.Now(),
			buffer:     make([]byte, 0),
		}
		s.sessions.Store(sessionID, session)
		if s.debug {
			log.Printf("[DEBUG] New session created and stored for %s", sessionID[:8])
		}
	} else {
		session = sessionInterface.(*Session)
		if session.conn == nil {
			if s.debug {
				log.Printf("[DEBUG] Session %s found but connection is nil, reconnecting", sessionID[:8])
			}
			conn, err := net.Dial("tcp", fmt.Sprintf("%s:%s", host, port))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			session.conn = conn
			if s.debug {
				log.Printf("[DEBUG] Reconnected session %s", sessionID[:8])
			}
		} else {
			if s.debug {
				log.Printf("[DEBUG] Reusing existing connection for session %s", sessionID[:8])
			}
		}
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	session.lastActive = time.Now()

	if r.Header.Get("X-Connection-Close") == "true" {
		if s.debug {
			log.Printf("[DEBUG] Closing connection for session %s", sessionID[:8])
		}
		session.conn.Close()
		session.conn = nil
		s.sessions.Delete(sessionID)
		return
	}

	if r.Method == http.MethodPost {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			if s.debug {
				log.Printf("Error reading request body: %v", err)
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(data) > 0 {
			if s.debug {
				log.Printf("POST: Writing %d bytes to connection for session %s",
					len(data),
					sessionID[:8], // First 8 chars of session ID for brevity
				)
			}
			_, err = session.conn.Write(data)
			if err != nil {
				if s.debug {
					log.Printf("Error writing to connection: %v", err)
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		return
	}

	// For GET requests, read any available data
	buffer := make([]byte, 32*1024)      // 32KB buffer
	readData := make([]byte, 0, 64*1024) // 64KB initial capacity

	for {
		session.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)) // Increased from 10ms to 100ms
		n, err := session.conn.Read(buffer)
		if err != nil {
			if err != io.EOF && !err.(net.Error).Timeout() {
				if s.debug {
					log.Printf("Error reading from connection: %v", err)
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			break
		}
		if n > 0 {
			readData = append(readData, buffer[:n]...)
		}
		if n < len(buffer) || len(readData) >= 64*1024 { // Added size limit check
			break
		}
	}

	// Only encode and send if we have data
	if len(readData) > 0 {
		encoded := hex.EncodeToString(readData)
		if s.debug {
			log.Printf("Response: Sending %d bytes (encoded: %d bytes) for session %s path %s",
				len(readData),
				len(encoded),
				sessionID[:8],
				r.URL.Path,
			)
		}
		w.Write([]byte(encoded))
	} else if s.debug {
		log.Printf("Response: No data to send for session %s path %s",
			sessionID[:8],
			r.URL.Path,
		)
	}
}

func main() {
	var origin string
	var certFile string
	var keyFile string
	var debug bool
	var allowDirect bool
	var appCommand string

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "DarkFlare Server - TCP-over-CDN tunnel server component\n")
		fmt.Fprintf(os.Stderr, "(c) 2024 Barrett Lyon\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s [options]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		fmt.Fprintf(os.Stderr, "  -o        Listen address for the server\n")
		fmt.Fprintf(os.Stderr, "            Format: proto://[host]:port\n")
		fmt.Fprintf(os.Stderr, "            Default: http://0.0.0.0:8080\n\n")
		fmt.Fprintf(os.Stderr, "  -allow-direct\n")
		fmt.Fprintf(os.Stderr, "            Allow direct connections not coming through Cloudflare\n")
		fmt.Fprintf(os.Stderr, "            Default: false (only allow Cloudflare IPs)\n\n")
		fmt.Fprintf(os.Stderr, "  -c        Path to TLS certificate file\n")
		fmt.Fprintf(os.Stderr, "            Default: Auto-generated self-signed cert\n\n")
		fmt.Fprintf(os.Stderr, "  -k        Path to TLS private key file\n")
		fmt.Fprintf(os.Stderr, "            Default: Auto-generated with cert\n\n")
		fmt.Fprintf(os.Stderr, "  -debug    Enable detailed debug logging\n")
		fmt.Fprintf(os.Stderr, "            Shows connection details and errors\n\n")
		fmt.Fprintf(os.Stderr, "Examples:\n")
		fmt.Fprintf(os.Stderr, "  Basic setup:\n")
		fmt.Fprintf(os.Stderr, "    %s -o http://0.0.0.0:8080\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  With custom TLS certificates:\n")
		fmt.Fprintf(os.Stderr, "    %s -o https://0.0.0.0:443 -c /path/to/cert.pem -k /path/to/key.pem\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  Debug mode with metrics:\n")
		fmt.Fprintf(os.Stderr, "    %s -o http://0.0.0.0:8080 -debug\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Notes:\n")
		fmt.Fprintf(os.Stderr, "  - Server accepts destination from client via X-Requested-With header\n")
		fmt.Fprintf(os.Stderr, "  - Destination validation is performed for security\n")
		fmt.Fprintf(os.Stderr, "  - Use with Cloudflare as reverse proxy for best security\n\n")
		fmt.Fprintf(os.Stderr, "For more information: https://github.com/doxx/darkflare\n")
	}

	flag.StringVar(&origin, "o", "http://0.0.0.0:8080", "")
	flag.StringVar(&certFile, "c", "", "")
	flag.StringVar(&keyFile, "k", "", "")
	flag.StringVar(&appCommand, "a", "", "")
	flag.BoolVar(&debug, "debug", false, "")
	flag.BoolVar(&allowDirect, "allow-direct", false, "")
	flag.Parse()

	// Parse origin URL
	originURL, err := url.Parse(origin)
	if err != nil {
		log.Fatalf("Invalid origin URL: %v", err)
	}

	// Validate scheme
	if originURL.Scheme != "http" && originURL.Scheme != "https" {
		log.Fatal("Origin scheme must be either 'http' or 'https'")
	}

	// Validate and extract host/port
	originHost, originPort, err := net.SplitHostPort(originURL.Host)
	if err != nil {
		log.Fatalf("Invalid origin address: %v", err)
	}

	// Validate IP is local
	if !isLocalIP(originHost) {
		log.Fatal("Origin host must be a local IP address")
	}

	server := NewServer(originHost, originPort, appCommand, debug, allowDirect)

	log.Printf("DarkFlare server running on %s://%s:%s", originURL.Scheme, originHost, originPort)
	if allowDirect {
		log.Printf("Warning: Direct connections allowed (no Cloudflare required)")
	}

	// Start server with appropriate protocol
	if originURL.Scheme == "https" {
		if certFile == "" || keyFile == "" {
			log.Fatal("HTTPS requires both certificate (-c) and key (-k) files")
		}

		// Load and verify certificates
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			log.Fatalf("Failed to load certificate and key: %v", err)
		}

		// Create a TLS session cache
		tlsSessionCache := tls.NewLRUClientSessionCache(1000) // Cache up to 1000 sessions

		server := &http.Server{
			Addr:    fmt.Sprintf("%s:%s", originHost, originPort),
			Handler: http.HandlerFunc(server.handleRequest),
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
				MaxVersion:   tls.VersionTLS13,
				// Disable HTTP/2
				NextProtos: []string{"http/1.1"},
				// Enable session tickets for session resumption
				SessionTicketsDisabled: false,
				// Use client session cache
				ClientSessionCache: tlsSessionCache,
				// Prefer server cipher suites
				PreferServerCipherSuites: true,
				// Let server choose cipher suites
				ClientAuth: func() tls.ClientAuthType {
					if server.allowDirect {
						return tls.NoClientCert
					}
					return tls.RequestClientCert
				}(),
				// Handle SNI
				GetCertificate: func(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
					if debug {
						log.Printf("Client requesting certificate for server name: %s", info.ServerName)
					}
					return &cert, nil
				},
				GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					if debug {
						log.Printf("TLS Handshake Details:")
						log.Printf("  Client Address: %s", hello.Conn.RemoteAddr())
						log.Printf("  Server Name: %s", hello.ServerName)
						log.Printf("  Supported Versions: %v", hello.SupportedVersions)
						log.Printf("  Supported Ciphers: %v", hello.CipherSuites)
						log.Printf("  Supported Curves: %v", hello.SupportedCurves)
						log.Printf("  Supported Points: %v", hello.SupportedPoints)
					}
					return nil, nil
				},
				VerifyConnection: func(cs tls.ConnectionState) error {
					if debug {
						log.Printf("TLS Connection State:")
						log.Printf("  Version: 0x%x", cs.Version)
						log.Printf("  HandshakeComplete: %v", cs.HandshakeComplete)
						log.Printf("  CipherSuite: 0x%x", cs.CipherSuite)
						log.Printf("  NegotiatedProtocol: %s", cs.NegotiatedProtocol)
						log.Printf("  ServerName: %s", cs.ServerName)
					}
					return nil
				},
			},
			ErrorLog: log.New(os.Stderr, "[HTTPS] ", log.LstdFlags),
			ConnState: func(conn net.Conn, state http.ConnState) {
				if debug {
					log.Printf("Connection state changed to %s from %s",
						state, conn.RemoteAddr().String())
				}
			},
			// Add timeouts to prevent hanging connections
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  120 * time.Second,
		}

		log.Printf("Starting HTTPS server on %s:%s", originHost, originPort)
		if debug {
			log.Printf("TLS Configuration:")
			log.Printf("  Minimum Version: %x", server.TLSConfig.MinVersion)
			log.Printf("  Maximum Version: %x", server.TLSConfig.MaxVersion)
			log.Printf("  Certificates Loaded: %d", len(server.TLSConfig.Certificates))
			log.Printf("  Listening Address: %s", server.Addr)
			log.Printf("  Supported Protocols: %v", server.TLSConfig.NextProtos)
		}

		log.Fatal(server.ListenAndServeTLS(certFile, keyFile))
	} else {
		server := &http.Server{
			Addr:    fmt.Sprintf("%s:%s", originHost, originPort),
			Handler: http.HandlerFunc(server.handleRequest),
		}
		log.Fatal(server.ListenAndServe())
	}
}

func isLocalIP(ip string) bool {
	if ip == "0.0.0.0" || ip == "127.0.0.1" || ip == "::1" {
		return true
	}

	ipAddr := net.ParseIP(ip)
	if ipAddr == nil {
		return false
	}

	// Check if IP is assigned to any local interface
	interfaces, err := net.Interfaces()
	if err != nil {
		return false
	}

	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			switch v := addr.(type) {
			case *net.IPNet:
				if v.IP.String() == ip {
					return true
				}
			case *net.IPAddr:
				if v.IP.String() == ip {
					return true
				}
			}
		}
	}

	// Also allow loopback and private IPs
	return ipAddr.IsLoopback() || ipAddr.IsPrivate()
}

func isValidDestination(dest string) bool {
	_, portStr, err := net.SplitHostPort(dest)
	if err != nil {
		return false
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return false
	}

	return true
}
