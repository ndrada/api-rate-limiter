// main package for standalone binary
package main

//import http, logging, contect packs and rate limiter
import (
	"context"  //token-bucket rate limiter
	"log"      //for simple logging of server start and errors
	"net/http" //provides http server and handles primitives

	"golang.org/x/time/rate"
)

//global process-wide rate limiter
//5 tokens per second & allow short bursts up to 10

var (
	ctx     = context.Background()
	limiter = rate.NewLimiter(5, 10) //how many reqs per sec
)

func rateLimitMiddleware(next http.Handler) http.Handler {
	//for writing inline handlers
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		//if no token, reject immediately
		if !limiter.Allow() {
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

// boot http server and install middleware
func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", helloHandler)   //register a single route
	handler := rateLimitMiddleware(mux) //wrap mux so every req is checked

	addr := ":8080" //address & port to listen to
	log.Printf("Server listening on %s (global limit: 5rps, burst 10)", addr)

	//start server
	log.Fatal(http.ListenAndServe(addr, handler))
}
