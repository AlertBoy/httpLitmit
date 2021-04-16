package main

import (
	"fmt"
	"golang.org/x/time/rate"
	"net/http"
	"sync"
)

/*限流器*/
type Limit struct {
	mu  *sync.RWMutex
	ips map[string]*rate.Limiter
	r   rate.Limit
	b   int
}
/*初始化一个limiter*/
func NewLimiter(r rate.Limit, b int) *Limit {
	return &Limit{
		ips: make(map[string]*rate.Limiter),
		mu:  &sync.RWMutex{},
		r:   r,
		b:   b,
	}
}
func (r *Limit) AddIP(ip string) (limiter *rate.Limiter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	limiter = rate.NewLimiter(r.r, r.b)
	r.ips[ip] = limiter
	return
}



func (r *Limit) GetLimiter(ip string) *rate.Limiter {
	r.mu.RLock()
	limiter, exists := r.ips[ip]

	if !exists {
		r.mu.RUnlock()
		return r.AddIP(ip)
	}

	r.mu.RUnlock()

	return limiter
}

/*每秒恢复一个桶，最多五个筒*/
var limiter = NewLimiter(0, 1)

func main() {
	http.HandleFunc("/", limitMiddleWare(okHandler))
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Println(err)
	}
}

/*限流中间件*/
func limitMiddleWare(handlerFunc http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limiter := limiter.GetLimiter(r.RemoteAddr)
		if !limiter.Allow() {
			http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)

			return
		}
		/*继续访问*/
		handlerFunc(w, r)
	}

}

func okHandler(w http.ResponseWriter, _ *http.Request) {
	// Some very expensive database call
	_, _ = w.Write([]byte("Get gut"))
}
