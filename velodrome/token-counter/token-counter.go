/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/google/go-github/github"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
)

type tokenCounterFlags struct {
	influx      InfluxConfig
	tokens      []string
	metricsPort int
	host        string
	promOnly    bool
}

func NewCollector(tokens []TokenHandler) prometheus.Collector {
	return &Collector{
		tokens: tokens,
		tokenCounter: prometheus.NewDesc(
			"github_token_count",
			"Number of counted API calls against the github API within a reset window",
			[]string{"login"}, nil),
	}
}

type Collector struct {
	tokenCounter *prometheus.Desc
	tokens       []TokenHandler
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.tokenCounter
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	for _, handler := range c.tokens {
		rate, err := handler.ProcessNow()
		if err != nil {
			logrus.WithError(err).WithField("login", handler.login).Errorf("Failed to fetch rate limits for login '%s'", handler.login)
			return
		}
		ch <- prometheus.MustNewConstMetric(c.tokenCounter, prometheus.GaugeValue, float64(rate.Limit-rate.Remaining), handler.login)
	}
}

func (flags *tokenCounterFlags) AddFlags(cmd *cobra.Command) {
	cmd.Flags().StringSliceVar(&flags.tokens, "token", []string{}, "List of tokens")
	cmd.Flags().IntVar(&flags.metricsPort, "metrics-port", 9090, "port to listen on for prometheus metrics scraping")
	cmd.Flags().StringVar(&flags.host, "host", "", "host address to listen on for prometheus metrics scraping")
	cmd.Flags().BoolVar(&flags.promOnly, "prom-only", false, "Only expose prometheus metrics, don't push to influxdb")
	cmd.Flags().AddGoFlagSet(flag.CommandLine)
}

// TokenHandler is refreshing token usage
type TokenHandler struct {
	gClient  *github.Client
	influxdb *InfluxDB
	login    string
}

// GetGithubClient creates a client for each token
func GetGithubClient(token string) *github.Client {
	return github.NewClient(
		oauth2.NewClient(
			oauth2.NoContext,
			oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}),
		),
	)
}

// GetUsername finds the login for each token
func GetUsername(client *github.Client) (string, error) {
	user, _, err := client.Users.Get(context.Background(), "")
	if err != nil {
		return "", err
	}
	if user.Login == nil {
		return "", errors.New("Users.Get(\"\") returned empty login")
	}

	return *user.Login, nil
}

// CreateTokenHandler parses the token and create a handler
func CreateTokenHandler(tokenStream io.Reader, influxdb *InfluxDB) (*TokenHandler, error) {
	token, err := ioutil.ReadAll(tokenStream)
	if err != nil {
		return nil, err
	}
	client := GetGithubClient(strings.TrimSpace(string(token)))
	login, err := GetUsername(client) // Get user name for token
	if err != nil {
		return nil, err
	}

	return &TokenHandler{
		gClient:  client,
		login:    login,
		influxdb: influxdb,
	}, nil
}

// CreateTokenHandlers goes through the list of token files, and create handlers
func CreateTokenHandlers(tokenFiles []string, influxdb *InfluxDB) ([]TokenHandler, error) {
	tokens := []TokenHandler{}
	for _, tokenFile := range tokenFiles {
		f, err := os.Open(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("Can't open token-file (%s): %s", tokenFile, err)
		}
		token, err := CreateTokenHandler(f, influxdb)
		if err != nil {
			return nil, fmt.Errorf("Failed to create token (%s): %s", tokenFile, err)
		}
		tokens = append(tokens, *token)
	}
	return tokens, nil
}

func (t TokenHandler) getCoreRate() (*github.Rate, error) {
	limits, _, err := t.gClient.RateLimits(context.Background())
	if err != nil {
		return nil, err
	}
	return limits.Core, nil
}

// Process does the main job:
// It tries to get the value of "Remaining" rate just before the token
// gets reset. It does that more and more often (as the reset date gets
// closer) to get the most accurate value.
func (t TokenHandler) Process() {
	lastRate, err := t.getCoreRate()
	if err != nil {
		glog.Fatalf("%s: Couldn't get rate limits: %v", t.login, err)
	}

	for {
		halfPeriod := lastRate.Reset.Time.Sub(time.Now()) / 2
		time.Sleep(halfPeriod)
		newRate, err := t.getCoreRate()
		if err != nil {
			glog.Error("Failed to get CoreRate: ", err)
			continue
		}
		// There is a bug in Github. They seem to reset the Remaining value before resetting the Reset value.
		if !newRate.Reset.Time.Equal(lastRate.Reset.Time) || newRate.Remaining > lastRate.Remaining {
			if err := t.influxdb.Push(
				"github_token_count",
				map[string]string{"login": t.login},
				map[string]interface{}{"value": lastRate.Limit - lastRate.Remaining},
				lastRate.Reset.Time,
			); err != nil {
				glog.Error("Failed to push count:", err)
			}
			// Make sure the timer is properly reset, and we have time anyway
			time.Sleep(30 * time.Minute)
			for {
				newRate, err = t.getCoreRate()
				if err == nil {
					break
				}
				glog.Error("Failed to get CoreRate: ", err)
				time.Sleep(time.Minute)
			}

		}
		lastRate = newRate
	}
}

func (t TokenHandler) ProcessNow() (*github.Rate, error) {
	newRate, err := t.getCoreRate()
	if err != nil {
		return nil, fmt.Errorf("failed to get CoreRate: %v", err)
	}
	return newRate, nil
}

func runProgram(flags *tokenCounterFlags) error {

	influxdb, err := flags.influx.CreateDatabaseClient()
	if err != nil {
		return err
	}

	tokens, err := CreateTokenHandlers(flags.tokens, influxdb)
	if err != nil {
		return err
	}

	if len(tokens) == 0 {
		glog.Warning("No token given, nothing to do. Leaving...")
		return nil
	}

	tokenCounter := NewCollector(tokens)
	prometheus.MustRegister(tokenCounter)
	// listen for prometheus scraping
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsAddr := fmt.Sprintf("%s:%d", flags.host, flags.metricsPort)
	go func() {
		logrus.Infof("Metrics Listening on: %s", metricsAddr)
		logrus.WithField("mux", "metrics").WithError(
			http.ListenAndServe(metricsAddr, metricsMux),
		).Fatal("ListenAndServe returned.")
	}()

	// if we only serve prometheus metrics, stop here
	if flags.promOnly {
		select {}
	}

	for _, token := range tokens {
		go token.Process()
	}

	select {}
}

func main() {
	flags := &tokenCounterFlags{}
	cmd := &cobra.Command{
		Use:   filepath.Base(os.Args[0]),
		Short: "Count usage of github token",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runProgram(flags)
		},
	}
	flags.AddFlags(cmd)
	flags.influx.AddFlags(cmd)

	if err := cmd.Execute(); err != nil {
		glog.Error(err)
	}
}
