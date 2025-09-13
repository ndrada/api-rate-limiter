// main package for standalone binary
package main

//import http, logging, contect packs and rate limiter
import (
	"context"  // to set a per-req wait deadline
	"log"      //for simple logging of server start and errors
	"math"     //for Floor/Ceil on token math
	"net"      //to extract remote ip
	"net/http" //provides http server and handles primitives
	"strconv"  // format header values
	"strings"  //small parsing helpers
	"sync"     // mutex to protect the map
	"time"     // timers & timestamps

	"golang.org/x/time/rate" //token-bucket rate limiter
)

// ROUTE LEVEL POLICYY DEFINITIONS

type RatePolicy struct {
	Name        string        //identify policy (used in limiter map key)
	RPS         int           //steady refill rate (tokens/sec)
	Burst       int           //short term capacity
	UseBlocking bool          //toggles blocking vs fast fail
	MaxWait     time.Duration //caps how long we'll wait in blocking mode
}

// matches reqs by method + path prefix & assigns a policy
type RouteRule struct {
	Method     string     //http method to match (empty = "any")
	PathPrefix string     //path prefix to match
	Policy     RatePolicy //policy to apply when rule matches
}

// for when no rule matches
var defaultPolicy = RatePolicy{
	Name:        "default",
	RPS:         5,
	Burst:       10,
	UseBlocking: true,
	MaxWait:     300 * time.Millisecond,
}

// ordered list (first match wins; top-down)
var routeRules = []RouteRule{
	//string login route
	{Method: "POST",
		PathPrefix: "/login",
		Policy: RatePolicy{
			Name:        "login",
			RPS:         1,
			Burst:       1,
			UseBlocking: true,
			MaxWait:     500 * time.Millisecond,
		},
	},
	//heavier endpoint: modest rate
	{
		Method:     "POST",
		PathPrefix: "/heavy",
		Policy: RatePolicy{
			Name:        "heavy",
			RPS:         2,
			Burst:       4,
			UseBlocking: false,
			MaxWait:     0,
		},
	},
	// add extra rules here depending on your api
}

// picks the first matching rule or returns default
func selectPolicy(r *http.Request) RatePolicy {
	//normalized method & path
	method := r.Method
	path := r.URL.Path

	for _, rr := range routeRules {
		if rr.Method != "" && rr.Method != method {
			continue
		}
		if rr.PathPrefix != "" && !strings.HasPrefix(path, rr.PathPrefix) {
			continue
		}
		return rr.Policy
	}
	return defaultPolicy
}

// PER CLIENT LIMITER STORE - keyes by client and policy

// store the limiter & when traffic was last seen for each client
type clientLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time //evicts idle clients so map doesn't grow forever
}

// threadsafe map of compositeKey -> clientLimiter
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

// combines client identity and policy name to isolat buckets/route
func keyFor(clientKey string, pol RatePolicy) string {
	return clientKey + "|" + pol.Name
}

// returns the limiter for a given client key (created on first use)
// updates lastSeen so cleanup knows this client is still active
func (s *limiterStore) get(clientKey string, pol RatePolicy) *rate.Limiter {
	composite := keyFor(clientKey, pol) //composite key

	//locking map while reading/updating
	s.mu.Lock()
	defer s.mu.Unlock()

	//check if a limiter for this complosite already exists
	//if yes: refresh last seen & return it
	if cl, ok := s.clients[composite]; ok {
		cl.lastSeen = time.Now()
		return cl.limiter
	}

	//if no: create a new limiter with the route's policy
	lim := rate.NewLimiter(rate.Limit(pol.RPS), pol.Burst)
	s.clients[composite] = &clientLimiter{
		limiter:  lim,
		lastSeen: time.Now(),
	}

	return lim
}

// launch background goroutine to periodically remove idle clients
func (s *limiterStore) startCleanup(idleTTL, interval time.Duration) {
	//background ticker loop
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			//for each tick, scan & delete entries
			cutoff := time.Now().Add(-idleTTL)

			s.mu.Lock() //scan under lock
			for key, cl := range s.clients {
				if cl.lastSeen.Before(cutoff) {
					delete(s.clients, key)
				}
			}
			s.mu.Unlock()
		}
	}()
}

//	EXTRACT CLIENT IDENTITY

// derives the client identity used for per client limits
func keyFromRequest(r *http.Request) string {
	// 1 - explicit API key (best)
	if apiKey := strings.TrimSpace(r.Header.Get("X-API-Key")); apiKey != "" {
		return "key:" + apiKey
	}

	//2 - first ip in X-Forwarded-For (when behind proxy)
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

// 	HELPERS

// rate limit headers on the response
func setRateHeaders(w http.ResponseWriter, pol RatePolicy, tokens float64) {
	if tokens < 0 {
		tokens = 0
	}
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(pol.RPS))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(int(math.Floor(tokens))))
}

// calculate est time to wait for 1 token based on current tokens
func retryAfterSeconds(pol RatePolicy, tokens float64) int {
	need := 1 - tokens
	if need < 0 {
		need = 0
	}
	if pol.RPS <= 0 {
		return 1
	}
	sec := int(math.Ceil(need / float64(pol.RPS)))
	if sec < 1 {
		sec = 1
	}
	return sec
}

//	MIDDLEWARE (per client, per route)

// enforces a policy chosen by route for each client
func rateLimitMiddleware(store *limiterStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pol := selectPolicy(r)           //pick route specific policy
		clientKey := keyFromRequest(r)   //figure out which client this req belongs to
		lim := store.get(clientKey, pol) //fetch or create limiter for client

		//blocking mode (if policy says block, wait up to Maxwait)
		if pol.UseBlocking {
			tokensBefore := lim.Tokens()                                 //grab current token estimate beofre making decision
			ctx, cancel := context.WithTimeout(r.Context(), pol.MaxWait) //per-req context cappwith policy's MaxWait
			defer cancel()

			//wait for token or timeout
			if err := lim.Wait(ctx); err != nil {
				//timed out or cancel => tell client to retry later
				setRateHeaders(w, pol, tokensBefore)
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(pol, tokensBefore)))
				http.Error(w, "Too many requests", http.StatusTooManyRequests)
				return
			}

			//token received & consumed => set headers using current tokens & pass through
			setRateHeaders(w, pol, lim.Tokens())
			next.ServeHTTP(w, r)
			return
		}
		//non blocking mode: fail fast w 429 when no token available
		tokensBefore := lim.Tokens()
		if !lim.Allow() {
			setRateHeaders(w, pol, tokensBefore)
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(pol, tokensBefore)))
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
			return
		}
		setRateHeaders(w, pol, lim.Tokens())
		next.ServeHTTP(w, r)
	})
}

//	DEMO HANDLERS

func helloHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

func heavyHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("heavy ok"))
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("login ok"))
}

// 	SERVER WIRING

func main() {
	store := newLimiterStore()
	store.startCleanup(5*time.Minute, 1*time.Minute)

	mux := http.NewServeMux()
	mux.HandleFunc("/", helloHandler)          //default pol
	mux.HandleFunc("/heavy", heavyHandler)     // GET /heavy
	mux.HandleFunc("/login", loginHandler)     // POST /login
	handler := rateLimitMiddleware(store, mux) //wrap mux so every req is checked

	addr := ":8080" //address & port to listen to
	log.Printf("Server listening on %s (route-level policies enabled)", addr)

	//start server
	log.Fatal(http.ListenAndServe(addr, handler))
}
