package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
	tasks "google.golang.org/api/tasks/v1"
)

// oauthScopes is the set of Google API scopes this assistant needs. Add new
// scopes here and delete token.json to force a re-consent.
var oauthScopes = []string{
	calendar.CalendarScope, // full calendar r/w
	tasks.TasksScope,      // full tasks r/w
}

// GetGoogleClients returns authenticated Google Calendar and Tasks services.
func GetGoogleClients(ctx context.Context) (*calendar.Service, *tasks.Service, error) {
	b, err := os.ReadFile("credentials.json")
	if err != nil {
		return nil, nil, fmt.Errorf("unable to read client secret file: %v", err)
	}
	config, err := google.ConfigFromJSON(b, oauthScopes...)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to parse client secret file to config: %v", err)
	}
	client := getClient(config)

	calSvc, err := calendar.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, nil, fmt.Errorf("calendar service: %v", err)
	}
	tasksSvc, err := tasks.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, nil, fmt.Errorf("tasks service: %v", err)
	}
	return calSvc, tasksSvc, nil
}

func getClient(config *oauth2.Config) *http.Client {
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
	}
	// Use a TokenSource that automatically refreshes the access token
	// using the refresh token, and persist the new token to disk.
	ts := config.TokenSource(context.Background(), tok)
	newTok, err := ts.Token()
	if err == nil && newTok.AccessToken != tok.AccessToken {
		saveToken(tokFile, newTok)
	}
	return oauth2.NewClient(context.Background(), &savingTokenSource{
		src:     ts,
		tokFile: tokFile,
		last:    newTok,
	})
}

// savingTokenSource wraps an oauth2.TokenSource and persists refreshed tokens.
type savingTokenSource struct {
	src     oauth2.TokenSource
	tokFile string
	last    *oauth2.Token
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	if tok.AccessToken != s.last.AccessToken {
		saveToken(s.tokFile, tok)
		s.last = tok
	}
	return tok, nil
}

// getTokenFromWeb spins up a one-shot local server on :8085 to catch Google's
// OAuth redirect, then exchanges the code for a token. Port 8085 is used so
// it doesn't collide with the main app server on :8080.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	config.RedirectURL = "http://localhost:8085"
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("\n🌐 Go to the following link in your browser:\n%v\n\n", authURL)

	codeCh := make(chan string)
	m := http.NewServeMux()
	srv := &http.Server{Addr: ":8085", Handler: m}

	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code != "" {
			fmt.Fprintf(w, "<h1>Authentication successful!</h1><p>You can close this tab and return to your terminal.</p>")
			codeCh <- code
		} else {
			fmt.Fprintf(w, "No code found in URL.")
		}
	})

	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	fmt.Println("⏳ Waiting for you to log in and authorize the app...")
	code := <-codeCh
	srv.Shutdown(context.Background())

	tok, err := config.Exchange(context.TODO(), code)
	if err != nil {
		log.Fatalf("Unable to retrieve token from web: %v", err)
	}
	return tok
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

func saveToken(path string, token *oauth2.Token) {
	fmt.Printf("💾 Saving credential file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}
