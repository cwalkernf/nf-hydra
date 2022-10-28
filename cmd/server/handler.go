/*
 * Copyright © 2015-2018 Aeneas Rekkas <aeneas+oss@aeneas.io>
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * @author		Aeneas Rekkas <aeneas+oss@aeneas.io>
 * @copyright 	2015-2018 Aeneas Rekkas <aeneas+oss@aeneas.io>
 * @license 	Apache-2.0
 */

package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/openziti/sdk-golang/ziti"
	"github.com/ory/x/servicelocatorx"

	"github.com/ory/x/corsx"
	"github.com/ory/x/httprouterx"

	analytics "github.com/ory/analytics-go/v4"
	"github.com/ory/x/configx"

	"github.com/ory/x/reqlog"

	"github.com/julienschmidt/httprouter"
	"github.com/rs/cors"
	"github.com/spf13/cobra"
	"github.com/urfave/negroni"
	"go.uber.org/automaxprocs/maxprocs"

	"github.com/ory/graceful"
	"github.com/ory/x/healthx"
	"github.com/ory/x/metricsx"
	"github.com/ory/x/networkx"
	"github.com/ory/x/otelx"

	"github.com/ory/hydra/client"
	"github.com/ory/hydra/consent"
	"github.com/ory/hydra/driver"
	"github.com/ory/hydra/driver/config"
	"github.com/ory/hydra/jwk"
	"github.com/ory/hydra/oauth2"
	"github.com/ory/hydra/x"
	prometheus "github.com/ory/x/prometheusx"
)

var _ = &consent.Handler{}

func EnhanceMiddleware(ctx context.Context, sl *servicelocatorx.Options, d driver.Registry, n *negroni.Negroni, address string, router *httprouter.Router, enableCORS bool, iface config.ServeInterface) http.Handler {
	if !networkx.AddressIsUnixSocket(address) {
		n.UseFunc(x.RejectInsecureRequests(d, d.Config().TLS(ctx, iface)))
	}

	for _, mw := range sl.HTTPMiddlewares() {
		n.UseFunc(mw)
	}

	n.UseHandler(router)
	corsx.ContextualizedMiddleware(func(ctx context.Context) (opts cors.Options, enabled bool) {
		return d.Config().CORS(ctx, iface)
	})

	return n
}

func isDSNAllowed(ctx context.Context, r driver.Registry) {
	if r.Config().DSN() == "memory" {
		r.Logger().Fatalf(`When using "hydra serve admin" or "hydra serve public" the DSN can not be set to "memory".`)
	}
}

func RunServeAdmin(slOpts []servicelocatorx.Option, dOpts []driver.OptionsModifier, cOpts []configx.OptionModifier) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		sl := servicelocatorx.NewOptions(slOpts...)

		d, err := driver.New(cmd.Context(), sl, append(dOpts, driver.WithOptions(configx.WithFlags(cmd.Flags()))))
		if err != nil {
			return err
		}
		isDSNAllowed(ctx, d)

		admin, _, adminmw, _ := setup(ctx, d, cmd)
		d.PrometheusManager().RegisterRouter(admin.Router)

		var wg sync.WaitGroup
		wg.Add(1)

		go serve(
			ctx,
			d,
			cmd,
			&wg,
			config.AdminInterface,
			EnhanceMiddleware(ctx, sl, d, adminmw, d.Config().ListenOn(config.AdminInterface), admin.Router, true, config.AdminInterface),
			d.Config().ListenOn(config.AdminInterface),
			d.Config().SocketPermission(config.AdminInterface),
		)

		wg.Wait()
		return nil
	}
}

func RunServePublic(slOpts []servicelocatorx.Option, dOpts []driver.OptionsModifier, cOpts []configx.OptionModifier) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		sl := servicelocatorx.NewOptions(slOpts...)

		d, err := driver.New(cmd.Context(), sl, append(dOpts, driver.WithOptions(configx.WithFlags(cmd.Flags()))))
		if err != nil {
			return err
		}
		isDSNAllowed(ctx, d)

		_, public, _, publicmw := setup(ctx, d, cmd)
		d.PrometheusManager().RegisterRouter(public.Router)

		var wg sync.WaitGroup
		wg.Add(1)

		go serve(
			ctx,
			d,
			cmd,
			&wg,
			config.PublicInterface,
			EnhanceMiddleware(ctx, sl, d, publicmw, d.Config().ListenOn(config.PublicInterface), public.Router, false, config.PublicInterface),
			d.Config().ListenOn(config.PublicInterface),
			d.Config().SocketPermission(config.PublicInterface),
		)

		wg.Wait()
		return nil
	}
}

func RunServeAll(slOpts []servicelocatorx.Option, dOpts []driver.OptionsModifier, cOpts []configx.OptionModifier) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		sl := servicelocatorx.NewOptions(slOpts...)

		d, err := driver.New(cmd.Context(), sl, append(dOpts, driver.WithOptions(configx.WithFlags(cmd.Flags()))))
		if err != nil {
			return err
		}

		admin, public, adminmw, publicmw := setup(ctx, d, cmd)

		d.PrometheusManager().RegisterRouter(admin.Router)
		d.PrometheusManager().RegisterRouter(public.Router)

		var wg sync.WaitGroup
		wg.Add(2)

		go serve(
			ctx,
			d,
			cmd,
			&wg,
			config.PublicInterface,
			EnhanceMiddleware(ctx, sl, d, publicmw, d.Config().ListenOn(config.PublicInterface), public.Router, false, config.PublicInterface),
			d.Config().ListenOn(config.PublicInterface),
			d.Config().SocketPermission(config.PublicInterface),
		)

		go serve(
			ctx,
			d,
			cmd,
			&wg,
			config.AdminInterface,
			EnhanceMiddleware(ctx, sl, d, adminmw, d.Config().ListenOn(config.AdminInterface), admin.Router, true, config.AdminInterface),
			d.Config().ListenOn(config.AdminInterface),
			d.Config().SocketPermission(config.AdminInterface),
		)

		wg.Wait()
		return nil
	}
}

func setup(ctx context.Context, d driver.Registry, cmd *cobra.Command) (admin *httprouterx.RouterAdmin, public *httprouterx.RouterPublic, adminmw, publicmw *negroni.Negroni) {
	fmt.Println(banner(config.Version))

	if d.Config().CGroupsV1AutoMaxProcsEnabled() {
		_, err := maxprocs.Set(maxprocs.Logger(d.Logger().Infof))

		if err != nil {
			d.Logger().WithError(err).Fatal("Couldn't set GOMAXPROCS")
		}
	}

	adminmw = negroni.New()
	publicmw = negroni.New()

	admin = x.NewRouterAdmin(d.Config().AdminURL)
	public = x.NewRouterPublic()

	adminLogger := reqlog.
		NewMiddlewareFromLogger(d.Logger(),
			fmt.Sprintf("hydra/admin: %s", d.Config().IssuerURL(ctx).String()))
	if d.Config().DisableHealthAccessLog(config.AdminInterface) {
		adminLogger = adminLogger.ExcludePaths("/admin"+healthx.AliveCheckPath, "/admin"+healthx.ReadyCheckPath)
	}

	adminmw.Use(adminLogger)
	adminmw.Use(d.PrometheusManager())

	publicLogger := reqlog.NewMiddlewareFromLogger(
		d.Logger(),
		fmt.Sprintf("hydra/public: %s", d.Config().IssuerURL(ctx).String()),
	)
	if d.Config().DisableHealthAccessLog(config.PublicInterface) {
		publicLogger.ExcludePaths(healthx.AliveCheckPath, healthx.ReadyCheckPath)
	}

	publicmw.Use(publicLogger)
	publicmw.Use(d.PrometheusManager())

	metrics := metricsx.New(
		cmd,
		d.Logger(),
		d.Config().Source(ctx),
		&metricsx.Options{
			Service: "ory-hydra",
			ClusterID: metricsx.Hash(fmt.Sprintf("%s|%s",
				d.Config().IssuerURL(ctx).String(),
				d.Config().DSN(),
			)),
			IsDevelopment: d.Config().DSN() == "memory" ||
				d.Config().IssuerURL(ctx).String() == "" ||
				strings.Contains(d.Config().IssuerURL(ctx).String(), "localhost"),
			WriteKey: "h8dRH3kVCWKkIFWydBmWsyYHR4M0u0vr",
			WhitelistedPaths: []string{
				"/admin" + jwk.KeyHandlerPath,
				jwk.WellKnownKeysPath,

				"/admin" + client.ClientsHandlerPath,
				client.DynClientsHandlerPath,

				oauth2.DefaultConsentPath,
				oauth2.DefaultLoginPath,
				oauth2.DefaultPostLogoutPath,
				oauth2.DefaultLogoutPath,
				oauth2.DefaultErrorPath,
				oauth2.TokenPath,
				oauth2.AuthPath,
				oauth2.LogoutPath,
				oauth2.UserinfoPath,
				oauth2.WellKnownPath,
				oauth2.JWKPath,
				"/admin" + oauth2.IntrospectPath,
				"/admin" + oauth2.DeleteTokensPath,
				oauth2.RevocationPath,

				"/admin" + consent.ConsentPath,
				"/admin" + consent.ConsentPath + "/accept",
				"/admin" + consent.ConsentPath + "/reject",
				"/admin" + consent.LoginPath,
				"/admin" + consent.LoginPath + "/accept",
				"/admin" + consent.LoginPath + "/reject",
				"/admin" + consent.LogoutPath,
				"/admin" + consent.LogoutPath + "/accept",
				"/admin" + consent.LogoutPath + "/reject",
				"/admin" + consent.SessionsPath + "/login",
				"/admin" + consent.SessionsPath + "/consent",

				healthx.AliveCheckPath,
				healthx.ReadyCheckPath,
				"/admin" + healthx.AliveCheckPath,
				"/admin" + healthx.ReadyCheckPath,
				healthx.VersionPath,
				"/admin" + healthx.VersionPath,
				prometheus.MetricsPrometheusPath,
				"/admin" + prometheus.MetricsPrometheusPath,
				"/",
			},
			BuildVersion: config.Version,
			BuildTime:    config.Date,
			BuildHash:    config.Commit,
			Config: &analytics.Config{
				Endpoint:             "https://sqa.ory.sh",
				GzipCompressionLevel: 6,
				BatchMaxSize:         500 * 1000,
				BatchSize:            250,
				Interval:             time.Hour * 24,
			},
		},
	)

	adminmw.Use(metrics)
	publicmw.Use(metrics)

	d.RegisterRoutes(ctx, admin, public)

	return
}

func serve(
	ctx context.Context,
	d driver.Registry,
	cmd *cobra.Command,
	wg *sync.WaitGroup,
	iface config.ServeInterface,
	handler http.Handler,
	address string,
	permission *configx.UnixPermission,
) {
	defer wg.Done()

	if tracer := d.Tracer(cmd.Context()); tracer.IsLoaded() {
		handler = otelx.TraceHandler(handler)
	}

	var tlsConfig *tls.Config
	if tc := d.Config().TLS(ctx, iface); tc.Enabled() {
		// #nosec G402 - This is a false positive because we use graceful.WithDefaults which sets the correct TLS settings.
		tlsConfig = &tls.Config{Certificates: GetOrCreateTLSCertificate(ctx, cmd, d, iface)}
	}

	var srv = graceful.WithDefaults(&http.Server{
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: time.Second * 5,
	})

	if err := graceful.Graceful(func() error {
		d.Logger().Infof("Setting up http server on %s", address)

		// --------------------- BEGIN_ZITIFICATION ---------------------- //
		//
		// -> First, check for "zitified" Bool parameter
		zitified, _ := cmd.Flags().GetBool("zitified")
		d.Logger().Infof("CW: Incoming config interface: %s", iface.Key("prefix"))

		// Check zitified bool is true, and interface is serve.admin:
		// Do not want to apply Zitification to Public listener
		var listener net.Listener

		if zitified && iface.Key("prefix") == "serve.admin.prefix" {
			// service := "nf-hydra-service"
			// zitiService := d.Config().ZITI_SERVICE()
			zitiService := os.Getenv("ZITI_SERVICE")

			if zitiService == "" {
				return errors.New("Zitified flag set, but ZITI_SERVICE environment variable not found")
			}

			d.Logger().Infof("Setting up Zitified listener on %s", zitiService)
			options := ziti.ListenOptions{
				ConnectTimeout: 5 * time.Minute,
				MaxConnections: 3,
			}
			var err error
			listener, err = ziti.NewContext().ListenWithOptions(zitiService, &options)

			if err != nil {
				return err
			}
		} else {
			d.Logger().Infof("Setting non Zitified listener")
			var err error
			listener, err = networkx.MakeListener(address, permission)
			if err != nil {
				return err
			}
		}
		// --------------------- END_ZITIFICATION ---------------------- //

		if networkx.AddressIsUnixSocket(address) {
			return srv.Serve(listener)
		}

		if tlsConfig != nil {
			return srv.ServeTLS(listener, "", "")
		}

		if iface == config.PublicInterface {
			d.Logger().Warnln("HTTPS is disabled. Please ensure that your proxy is configured to provide HTTPS, and that it redirects HTTP to HTTPS.")
		}

		return srv.Serve(listener)
	}, srv.Shutdown); err != nil {
		d.Logger().WithError(err).Fatal("Could not gracefully run server")
	}
}
