// main package for standalone binary
package main

//import http, logging, contect packs and rate limiter
import (
	"context"  // to set a per-req wait deadline
	"log"      //for simple logging of server start and errors
	"math"     //for Floor/Ceil on token math
	"net"      //to extract remote ip
	"net/http" //provides http server and handles primitives
	"strconv"
	"strings" //small parsing helpers
	"sync"    // mutex to protect the map
	"time"    // timers & timestamps

	"golang.org/x/time/rate" //token-bucket rate limiter
)

// per client rate policy
const (
	perClientRPS   = 5  //steady tokens/sec
	perClientBurst = 10 //burst capacity
)

// behavior toggle
var (
	useBlocking = true
	maxWait     = 300 * time.Millisecond //cap for length of wait per req
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

// helper: set standard rate limit headers on the response
func setRateHeaders(w http.ResponseWriter, tokens float64) {
	if tokens < 0 {
		tokens = 0
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(perClientRPS))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(int(math.Floor(tokens))))
}

// helper: estimate time to wiat for 1 token based on current tokens
func retryAfterSeconds(tokens float64) int {
	need := 1 - tokens
	if need < 0 {
		need = 0
	}
	if perClientRPS <= 0 {
		return 1
	}
	sec := int(math.Ceil(need / float64(perClientRPS)))
	if sec < 1 {
		sec = 1
	}
	return sec
}

// middleware: per client limiter w/ 2 behaviors (blockking or fail-fast)
// + headers (success & 429)
func rateLimitMiddleware(store *limiterStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keyFromRequest(r) //figure out which client this req belongs to
		lim := store.get(key)    //fetch or create limiter for client

		//blocking mode
		if useBlocking {
			tokensBefore := lim.Tokens()                             //grab current token estimate beofre making decision
			ctx, cancel := context.WithTimeout(r.Context(), maxWait) //per-req context capped by maxWait
			defer cancel()

			//wait for token or timeout
			if err := lim.Wait(ctx); err != nil {
				//timed out or cancel => tell client to retry later
				setRateHeaders(w, tokensBefore)
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(tokensBefore)))
				http.Error(w, "Too many requests", http.StatusTooManyRequests)
				return
			}

			//token received & consumed => set headers using current tokens & pass through
			setRateHeaders(w, lim.Tokens())
			next.ServeHTTP(w, r)
			return
		}
		//non blocking mode: fail fast w 429 when no token available
		tokensBefore := lim.Tokens()
		if !lim.Allow() {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(tokensBefore)))
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
			return
		}
		setRateHeaders(w, lim.Tokens())
		next.ServeHTTP(w, r)
	})
}

// for observing throttling behavior
func helloHandler(w http.ResponseWriter, r *http.Request) {
	//success message
	w.Write([]byte("ok"))
}

// wires everything together & starts the server
func main() {
	store := newLimiterStore()
	store.startCleanup(5*time.Minute, 1*time.Minute)

	mux := http.NewServeMux()
	mux.HandleFunc("/", helloHandler)          //register a single route
	handler := rateLimitMiddleware(store, mux) //wrap mux so every req is checked

	addr := ":8080" //address & port to listen to
	log.Printf("Server listening on %s (per-client: %drps, burst %d, blocking=%v, maxWait=%s)", addr, perClientRPS, perClientBurst, useBlocking, maxWait)

	//start server
	log.Fatal(http.ListenAndServe(addr, handler))
}
