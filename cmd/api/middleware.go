package main

import (
	"errors"
	"expvar"
	"fmt"
	"greenlight/internal/data"
	"greenlight/internal/validator"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tomasen/realip"

	"github.com/felixge/httpsnoop"
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
			ip := realip.FromRequest(r)

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

// authenticate is a middleware function that handles authentication for incoming HTTP requests.
// It checks the Authorization header in the request and validates the token.
// If the token is valid, it retrieves the user associated with the token and sets it in the request context.
// If the token is not provided or invalid, it returns an appropriate error response.
// The next handler is then called to continue processing the request.
func (app *application) autheticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Add the "Vary: Authorization" header to the response. This indicates to any
		// caches that the response may vary based on the value of the Authorization
		w.Header().Add("Vary", "Authorization")

		// Retrieve the value of the Authorization header from the request.
		// This will return the empty string "" if there is no such header found.
		authorizationHeader := r.Header.Get("Authorization")

		if authorizationHeader == "" {
			r = app.contextSetUser(r, data.AnonymousUser)
			next.ServeHTTP(w, r)
			return
		}

		headerParts := strings.Split(authorizationHeader, " ")
		if len(headerParts) != 2 || headerParts[0] != "Bearer" {
			app.invalidAuthenticationTokenResponse(w, r)
			return
		}

		token := headerParts[1]

		v := validator.New()

		if data.VaidateTokenPlaintext(v, token); !v.Valid() {
			app.invalidAuthenticationTokenResponse(w, r)
			return
		}

		user, err := app.models.Users.GetForToken(data.ScopeAuthentication, token)
		if err != nil {
			switch {
			case errors.Is(err, data.ErrRecordNotFound):
				app.invalidAuthenticationTokenResponse(w, r)
			default:
				app.serverErrorResponse(w, r, err)
			}
			return
		}

		r = app.contextSetUser(r, user)

		next.ServeHTTP(w, r)
	})
}

// requireAuthenticatedUser is a middleware function that checks if the user is authenticated before allowing access to the next handler.
// If the user is anonymous, it sends an authentication required response.
// It takes the next http.HandlerFunc as a parameter and returns an http.HandlerFunc.
func (app *application) requireAuthenticatedUser(next http.HandlerFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := app.contextGetUser(r)

		if user.IsAnonymous() {
			app.authenticationRequiredResponse(w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// requireActivatedUser is a middleware function that checks if the user is activated before allowing access to the next handler.
// If the user is not activated, it responds with an inactive account response.
// It takes the next http.HandlerFunc as a parameter and returns an http.HandlerFunc.
func (app *application) requireActivatedUser(next http.HandlerFunc) http.HandlerFunc {
	fn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := app.contextGetUser(r)

		if !user.Activated {
			app.inactiveAccountResponse(w, r)
			return
		}

		next.ServeHTTP(w, r)
	})

	return app.requireAuthenticatedUser(fn)
}

// requirePermission is a middleware function that checks if the user has the required permission code.
// It takes a permission code and a http.HandlerFunc as parameters and returns a http.HandlerFunc.
// The middleware function checks if the user has the permission code by retrieving all permissions for the user.
// If there is an error retrieving the permissions, it returns a server error response.
// If the user does not have the required permission, it returns a not permitted response.
// Otherwise, it calls the next http.HandlerFunc in the chain.
// This middleware function also requires the user to be activated, as enforced by the app.requireActivatedUser middleware.
func (app *application) requirePermission(code string, next http.HandlerFunc) http.HandlerFunc {
	fn := func(w http.ResponseWriter, r *http.Request) {
		user := app.contextGetUser(r)

		Permissions, err := app.models.Permissions.GetAllForUser(user.ID)
		if err != nil {
			app.serverErrorResponse(w, r, err)
			return
		}

		if !Permissions.Include(code) {
			app.notPermittedResponse(w, r)
			return
		}

		next.ServeHTTP(w, r)
	}
	return app.requireActivatedUser(fn)
}

// enableCORS is a middleware function that enables Cross-Origin Resource Sharing (CORS) for the API.
// It adds the necessary headers to the response to allow requests from trusted origins.
// The trusted origins are defined in the application configuration.
// If the request origin is found in the trusted origins list, the "Access-Control-Allow-Origin" header is set to the request origin.
// This middleware function should be used in the request chain to handle CORS before passing the request to the next handler.
func (app *application) enableCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")

		w.Header().Add("Vary", "Access-Control-Request-Method")

		origin := r.Header.Get("Origin")

		if origin != "" && len(app.config.cors.trustedOrigins) != 0 {
			for i := range app.config.cors.trustedOrigins {
				if origin == app.config.cors.trustedOrigins[i] {
					w.Header().Set("Access-Control-Allow-Origin", origin)

					if r.Method == http.MethodOptions &&
						r.Header.Get("Access-Control-Request-method") != "" {
						w.Header().Set("Access-Control-Allow-Methods", "OPTIONS, PUT, PATCH, DELETE")
						w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
						w.WriteHeader(http.StatusOK)
						return
					}
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (app *application) metrics(next http.Handler) http.Handler {
	// Initialize the new expvar variables when the middleware chain is first built.
	totalRequestsReceived := expvar.NewInt("total_requests_received")
	totalResponseSent := expvar.NewInt("total_responses_sent")
	totalProcessingTimeMicroseconds := expvar.NewInt("total_processing_time_micro_seconds")
	totalResonsesSentByStatus := expvar.NewMap("total_reponses_sent_by_status")

	// The following code will be run for every request ...
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Use the Add() method to increment the number of requests received by 1.
		totalRequestsReceived.Add(1)

		metrics := httpsnoop.CaptureMetrics(next, w, r)

		// On the way back up the middleware chain, increment the number of reponses
		// sent by 1.
		totalResponseSent.Add(1)

		totalProcessingTimeMicroseconds.Add(metrics.Duration.Microseconds())

		totalResonsesSentByStatus.Add(strconv.Itoa(metrics.Code), 1)
	})

}
