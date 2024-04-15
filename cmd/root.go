package cmd

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/impersonate"
)

type contextKey string

const contextKeyError = contextKey("error")

func reverseProxyDirector(originalDirector func(*http.Request), tokenSource oauth2.TokenSource, proxyTargetUrl *url.URL) func(*http.Request) {
	return func(req *http.Request) {
		originalDirector(req)
		req.Header.Set("Host", proxyTargetUrl.Host)
		req.Host = proxyTargetUrl.Host
		token, err := tokenSource.Token()
		if err != nil {
			*req = *req.WithContext(context.WithValue(req.Context(), contextKeyError, fmt.Errorf("get token: %w", err)))
			token = &oauth2.Token{}
		} else if token.AccessToken == "" {
			*req = *req.WithContext(context.WithValue(req.Context(), contextKeyError, fmt.Errorf("empty token")))
		}
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}
}

func newImpersonateServiceAccountTokenSource(ctx context.Context, audience string, targetPrincipal string) (oauth2.TokenSource, error) {
	tokenSource, err := impersonate.IDTokenSource(ctx, impersonate.IDTokenConfig{
		Audience:        audience,
		TargetPrincipal: targetPrincipal,
		IncludeEmail:    true,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating impersonate token source: %w", err)
	}

	return tokenSource, nil
}

func NewCmd() *cobra.Command {
	var cloudRunUrl string
	var impersonateServiceAccount string
	cmd := &cobra.Command{
		Use:   "ara-cloud-run --cloud-run-url URL [--impersonate-service-account SERVICE_ACCOUNT_EMAIL] command...",
		Short: "Running Ansible while using ARA hosted on Cloud Run",
		Long: `ara-cloud-run is a wrapper around the ansible command that sets up a reverse proxy to an ARA instance hosted on Google Cloud Run.
				It requires the gcloud command to be installed and configured.`,
		Run: func(cmd *cobra.Command, args []string) {
			ctx, cancel := signal.NotifyContext(context.Background(),
				syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			err := run(ctx, cloudRunUrl, impersonateServiceAccount, args)
			if exitError, ok := err.(*exec.ExitError); ok {
				os.Exit(exitError.ExitCode())
			} else if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "%+v\n", err)
				os.Exit(1)
			}
		},
		Args: cobra.MinimumNArgs(1),
	}

	cmd.Flags().StringVarP(&cloudRunUrl, "cloud-run-url", "u", "", "The URL of the Cloud Run service hosting ARA (Env: ARA_CLOUD_RUN_URL)")
	cmd.Flags().StringVar(&impersonateServiceAccount, "impersonate-service-account", "", "Impersonate the provided service account when calling the Cloud Run service (Env: ARA_CLOUD_RUN_IMPERSONATE_SERVICE_ACCOUNT)")
	return cmd
}

func run(ctx context.Context, cloudRunUrl string, impersonateServiceAccount string, args []string) error {
	var env []string

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen error: %w", err)
	}
	env = append(env, "ARA_API_CLIENT=http")
	env = append(env, fmt.Sprintf("ARA_API_SERVER=http://%s", listener.Addr()))
	randomPasswordBytes := make([]byte, 16)
	if _, err := rand.Read(randomPasswordBytes); err != nil {
		return fmt.Errorf("generate random password error: %w", err)
	}
	randomPassword := fmt.Sprintf("%x", randomPasswordBytes)
	env = append(env, "ARA_API_USERNAME=ara")
	env = append(env, fmt.Sprintf("ARA_API_PASSWORD=%s", randomPassword))

	if cloudRunUrl == "" {
		cloudRunUrl = os.Getenv("ARA_CLOUD_RUN_URL")
		if cloudRunUrl == "" {
			return fmt.Errorf("cloud run url is not set")
		}
	}

	targetUrl, err := url.Parse(cloudRunUrl)
	if err != nil {
		return fmt.Errorf("parse cloud run url: %w", err)
	}

	var tokenSource oauth2.TokenSource
	if impersonateServiceAccount == "" {
		impersonateServiceAccount = os.Getenv("ARA_CLOUD_RUN_IMPERSONATE_SERVICE_ACCOUNT")
	}
	if impersonateServiceAccount != "" {
		var err error
		tokenSource, err = newImpersonateServiceAccountTokenSource(ctx, fmt.Sprintf("%s://%s/", targetUrl.Scheme, targetUrl.Host), impersonateServiceAccount)
		if err != nil {
			return err
		}
	} else {
		tokenSource, err = idtoken.NewTokenSource(ctx, fmt.Sprintf("%s://%s/", targetUrl.Scheme, targetUrl.Host))
		if err != nil {
			return fmt.Errorf("get token source from default credentials: %w", err)
		}
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(targetUrl)
	reverseProxy.Director = reverseProxyDirector(reverseProxy.Director, tokenSource, targetUrl)
	reverseProxy.ModifyResponse = func(response *http.Response) error {
		err := response.Request.Context().Value(contextKeyError)
		if err != nil {
			return fmt.Errorf("proxy error: %s", err)
		}
		return nil
	}
	reverseProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	server := &http.Server{}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userName, password, ok := r.BasicAuth()
		if ok && userName == "ara" && password == randomPassword {
			reverseProxy.ServeHTTP(w, r)
		} else {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
		}
	})
	defer server.Close()
	go func() {
		err := server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s/api/", listener.Addr()), nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.SetBasicAuth("ara", randomPassword)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request to ARA API: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ara API returns unexpceted status code(%d)", resp.StatusCode)
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}
