# API Rate Limiter (Go) : Per-Client Token Bucket

Lightweight HTTP middleware that enforces **per-client** rate limits using Go’s token-bucket (`golang.org/x/time/rate`).  
Clients are identified by **`X-API-Key`**, then **`X-Forwarded-For`** / **`X-Real-IP`**, falling back to the remote IP.

---

## Features
- per-client limiter (`key → *rate.Limiter`) with idle eviction
- friendly **rate-limit headers** on all responses:
  - `X-RateLimit-Limit`: configured steady rate (req/sec)
  - `X-RateLimit-Remaining`: approx tokens left
  - `Retry-After`: on `429`, seconds to wait before retrying
- minimal drop-in **middleware** for `net/http`.

---

## Quick start

```bash
# Run (dev)
go run .

# Build binary (Windows example)
go build -o bin/rl-demo.exe
```

*Server listens on **:8080** and exposes a single **GET /** endpoint that replies with ok (when allowed)*

---

## Testing
### 1. By hand
```bash
# Will show headers
curl.exe -i http://localhost:8080/
```

### 2. Force a 429
```bash
# Will return Retry-After
1..30 | % { curl.exe -i -s http://localhost:8080/ | Select-String "HTTP/|X-Rate|Retry-After" }
```

### 3. Load test with hey 
```bash
# Install & add to path
go install github.com/rakyll/hey@latest
$env:Path += ";$env:USERPROFILE\go\bin"

# Hammer test
hey -n 200 -c 20 http://localhost:8080/

# Paced test
hey -z 10s -q 5 -c 2 http://localhost:8080/

# Per client fairness
# Terminal A
hey -z 10s -q 5 -c 2 -H "X-API-Key: A" http://localhost:8080/

# Terminal B
hey -z 10s -q 5 -c 2 -H "X-API-Key: B" http://localhost:8080/
```

## How it works
- for each client key there is a token-bucket limiter
- every request: 
    - derives client key -> fetches/creates limiter
    - if **Allow()** succeeds -> **X-RateLimit-** is set 
    - if not -> **X-RateLimit-** + **Retry-After** are set and a 429 is returned
- a background go routine evicts idle clients from map

## Next steps/ideas
- add blocking mode (use limiter.Wait to delay)
- route-level configs (different limits per endpoint)
- metrics for allowed/denied/latency
- redis backend ? to share limits across instances
- maybe turn this into a cli or small library package for drop-in use




