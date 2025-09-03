// main package for standalone binary
package main

//import http, logging, contect packs and rate limiter
import (
	"log"      //for simple logging of server start and errors
	"net"      //to extract remote ip
	"net/http" //provides http server and handles primitives
	"strings"  //small parsing helpers
	"sync"     // mutex to protect the map
	"time"     // timers & timestamps

	"golang.org/x/time/rate" //token-bucket rate limiter
)

// per client rate policy
const (
	perClientRPS   = 5  //steady tokens/sec
	perClientBurst = 10 //burst capacity
)

// store the limiter & when traffic was last seen for each client
type clientLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time //evicts idle clients so map doesn't grow forever
}

// threadsafe map of clientKey -> clientLimiter
type limiterStore struct {
	mu      sync.Mutex //guard because handlers run concurrently
	clients map[string]*clientLimiter
}

// constructs store with empty map
func newLimiterStore() *limiterStore {
	return &limiterStore{
		clients: make(map[string]*clientLimiter),
	}
}

// returns the limiter for a given client key (created on first use)
// updates lastSeen so cleanup knows this client is still active
func (s *limiterStore) get(key string) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()

	//check if a limiter for this key already exists
	//if yes: refresh last seen & return it
	if cl, ok := s.clients[key]; ok {
		cl.lastSeen = time.Now()
		return cl.limiter
	}

	//if no: create a new token-bucket limiter for this client
	lim := rate.NewLimiter(perClientRPS, perClientBurst)
	s.clients[key] = &clientLimiter{
		limiter:  lim,
		lastSeen: time.Now(),
	}

	return lim
}

// launch background goroutine to periodically remove idle clients
func (s *limiterStore) startCleanup(idleTTL time.Duration, interval time.Duration) {
	//background ticker loop
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			//for each tick, scan & delete entries
			cutoff := time.Now().Add(-idleTTL)

			s.mu.Lock()
			for key, cl := range s.clients {
				if cl.lastSeen.Before(cutoff) {
					delete(s.clients, key)
				}
			}
			s.mu.Unlock()
		}
	}()
}

//extract client identity
/*
PRIORITY:
	1. X-API-Key header (explicit identity)
	2. X-Forwarded-for (first IP) if behind a proxy
	3. X-Real-IP
	4. RemoteAddr IP (direct client)
*/
func keyFromRequest(r *http.Request) string {
	// 1 - explicit identity
	if apiKey := strings.TrimSpace(r.Header.Get("X-API-Key")); apiKey != "" {
		return "key:" + apiKey
	}

	//2 - behind proxy
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return "ip:" + ip
			}
		}
	}

	//3 - some proxies use x real ip for the original client ip
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return "ip:" + xrip
	}

	//4 - fallback to the remote address on the connection
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "ip:" + r.RemoteAddr
	}

	return "ip:" + host
}

// this now uses per client store instead of global
func rateLimitMiddleware(store *limiterStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keyFromRequest(r)
		lim := store.get(key)

		//if no token, reject immediately
		if !lim.Allow() {
			//tell client limit exceeded
			http.Error(w, "Too many requests, amigo", http.StatusTooManyRequests)
			//stop so the downstream handler does not run
			return
		}

		//if there is a token, pass the rew to next handler
		next.ServeHTTP(w, r)
	})
}

// for observing throttling behavior
func helloHandler(w http.ResponseWriter, r *http.Request) {
	//success message
	w.Write([]byte("ok"))
}

// creates per client store
// starts cleanup goroutine to evict idle clients
// mounts middleware
// starts server
func main() {
	store := newLimiterStore()
	store.startCleanup(5*time.Minute, 1*time.Minute)

	mux := http.NewServeMux()
	mux.HandleFunc("/", helloHandler)          //register a single route
	handler := rateLimitMiddleware(store, mux) //wrap mux so every req is checked

	addr := ":8080" //address & port to listen to
	log.Printf("Server listening on %s (per-client: %drps, burst %d)", addr, perClientRPS, perClientBurst)

	//start server
	log.Fatal(http.ListenAndServe(addr, handler))
}
