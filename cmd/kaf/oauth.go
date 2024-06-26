package main

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/IBM/sarama"
	aws_signer "github.com/aws/aws-msk-iam-sasl-signer-go/signer"
	"github.com/aws/aws-sdk-go-v2/aws"
	aws_config "github.com/aws/aws-sdk-go-v2/config"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

var (
	once              sync.Once
	tokenProv         *tokenProvider
	refreshBuffer     time.Duration = time.Second * 20
	tokenFetchTimeout time.Duration = time.Second * 10
)

var _ sarama.AccessTokenProvider = &tokenProvider{}

type tokenProvider struct {
	// refreshMutex is used to ensure that tokens are not refreshed concurrently.
	refreshMutex sync.Mutex
	// The time at which the token expires.
	expiresAt time.Time
	// The time at which the token should be replaced.
	replaceAt time.Time
	// The currently cached token value.
	currentToken string
	// ctx for token fetching
	ctx context.Context
	// cfg for token fetching from
	oauthClientCFG *clientcredentials.Config
	// static token
	staticToken bool
}

// This is a singleton
func newTokenProvider() *tokenProvider {
	once.Do(func() {
		cluster := currentCluster
		ctx := context.Background()

		// token either from tokenURL, static or AWS API
		if cluster.SASL.Mechanism == "AWS_MSK_IAM" {
			var cfg aws.Config
			var err error
			if cluster.SASL.Profile != "" {
				cfg, err = aws_config.LoadDefaultConfig(ctx,
					aws_config.WithSharedConfigProfile(cluster.SASL.Profile),
				)
			} else {
				cfg, err = aws_config.LoadDefaultConfig(ctx)
			}
			if err != nil {
				errorExit("Could not load AWS config: " + err.Error())
			}
			token, _, err := aws_signer.GenerateAuthTokenFromCredentialsProvider(ctx, cfg.Region, cfg.Credentials)
			if err != nil {
				errorExit("Could not generate auth token: " + err.Error())
			}
			tokenProv = &tokenProvider{
				oauthClientCFG: &clientcredentials.Config{},
				staticToken:    true,
				currentToken:   token,
			}
		} else if len(cluster.SASL.Token) != 0 {
			tokenProv = &tokenProvider{
				oauthClientCFG: &clientcredentials.Config{},
				staticToken:    true,
				currentToken:   cluster.SASL.Token,
			}
		} else {
			tokenProv = &tokenProvider{
				oauthClientCFG: &clientcredentials.Config{
					ClientID:     cluster.SASL.ClientID,
					ClientSecret: cluster.SASL.ClientSecret,
					TokenURL:     cluster.SASL.TokenURL,
					Scopes:       cluster.SASL.Scopes,
				},
				staticToken: false,
			}
		}
		if !tokenProv.staticToken {
			// create context with timeout
			httpClient := &http.Client{Timeout: tokenFetchTimeout}
			ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)
			tokenProv.ctx = ctx

			// get first token
			firstToken, err := tokenProv.oauthClientCFG.Token(ctx)
			if err != nil {
				errorExit("Could not fetch OAUTH token: " + err.Error())
			}
			tokenProv.currentToken = firstToken.AccessToken
			tokenProv.expiresAt = firstToken.Expiry
			tokenProv.replaceAt = firstToken.Expiry.Add(-refreshBuffer)
		}
	})
	return tokenProv
}

func (tp *tokenProvider) Token() (*sarama.AccessToken, error) {

	if !tp.staticToken {
		if time.Now().After(tp.replaceAt) {
			if err := tp.refreshToken(); err != nil {
				return nil, err
			}

		}
	}
	return &sarama.AccessToken{
		Token:      tp.currentToken,
		Extensions: nil,
	}, nil
}

func (tp *tokenProvider) refreshToken() error {
	// Get a lock on the update
	tp.refreshMutex.Lock()
	defer tp.refreshMutex.Unlock()

	// Check whether another call refreshed the token while waiting for the lock to be acquired here
	if time.Now().Before(tp.replaceAt) {
		return nil
	}

	token, err := tp.oauthClientCFG.Token(tp.ctx)
	if err != nil {
		return err
	}
	// Save the token
	tp.currentToken = token.AccessToken
	tp.expiresAt = token.Expiry
	tp.replaceAt = token.Expiry.Add(-refreshBuffer)
	return nil
}
