package main

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// recoverPanic is a middleware function that recovers from panics in the application and sends a server error response.
// It wraps the provided http.Handler and adds a deferred function to recover from any panics that occur during the execution of the handler.
// If a panic occurs, it sets the "Connection" header to "close" and calls the serverErrorResponse method of the application to send a server error response.
// The recovered error is passed as the error parameter to the serverErrorResponse method.
// Parameters:
// - next: The http.Handler to wrap and execute.
// Returns:
// - http.Handler: The wrapped http.Handler.
func (app *application) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				w.Header().Set("Connection", "close")
				app.serverErrorResponse(w, r, fmt.Errorf("%s", err))
			}
		}()

		next.ServeHTTP(w, r)
	})
}

// rateLimit is a middleware function that implements rate limiting for incoming HTTP requests.
// It restricts the number of requests that can be made within a certain time period for each client IP address.
// The rate limit is based on a token bucket algorithm using the "golang.org/x/time/rate" package.
// The function takes an http.Handler as input and returns an http.Handler.
// It checks if rate limiting is enabled in the application configuration and extracts the client's IP address from the request.
// If the IP address is not found in the clients map, a new rate limiter is initialized and added to the map.
// The last seen time for the client is updated and the Allow() method is called on the rate limiter.
// If the request is not allowed, a 429 Too Many Requests response is sent.
// The function also includes a background goroutine that removes old entries from the clients map every minute.
// This ensures that IP addresses that have not been seen within the last three minutes are removed from the map.
// The rateLimit function is designed to be used as middleware in an HTTP server.
func (app *application) rateLimit(next http.Handler) http.Handler {

	// Delare a mutex and a map to hold the clients IP address and rate limiters.
	type client struct {
		limiter  *rate.Limiter
		lastSeen time.Time
	}

	var (
		mu      sync.Mutex
		clients = make(map[string]*client)
	)

	// Launch a background goroutine which removes old entries from the clients
	// map once every minute.
	go func() {
		for {
			time.Sleep(time.Minute)

			// Lock the mutex to prevent any rate limiter checks from happening while
			// the cleanup is taking place.
			mu.Lock()

			// Loop through all clients. If they haven't been seen within the
			// last three minutes, delete the corresponding entry from the map.
			for ip, client := range clients {
				if time.Since(client.lastSeen) > 3*time.Minute {
					delete(clients, ip)
				}
			}

			// Importantly, unloack the mutex when the cleanup is complete.
			mu.Unlock()

		}
	}()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only carry out the check if rate limiring is enabled.
		if app.config.limiter.enabled {
			// Extract the client's IP adress from the request.
			ip, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				app.serverErrorResponse(w, r, err)
				return
			}

			// Lock the mutex to prevent this code from being executed concurrently.
			mu.Lock()

			// Check to see if the IP address already exists in the map. If it doesn't then
			// initialize a new rate limiter and add the IP address and limiter to the map.
			if _, found := clients[ip]; !found {
				clients[ip] = &client{
					limiter: rate.NewLimiter(
						rate.Limit(app.config.limiter.rps),
						app.config.limiter.burst),
				}
			}

			// Update the last seen time for the client.
			clients[ip].lastSeen = time.Now()

			// Call the Allow() method on the rate limiter for the current IP address. If
			// the request isn't allowed, unlock the mutex and send a 429 Too Many requests reponse
			if !clients[ip].limiter.Allow() {
				mu.Unlock()
				app.rateLimitExceededResponse(w, r)
				return
			}

			mu.Unlock()
		}

		next.ServeHTTP(w, r)
	})

}
