package cobrautil

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/jzelinskie/stringz"
	"github.com/mattn/go-isatty"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/jaeger"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// IsBuiltinCommand checks against a hard-coded list of the names of commands
// that cobra provides out-of-the-box.
func IsBuiltinCommand(cmd *cobra.Command) bool {
	return stringz.SliceContains([]string{
		"help [command]",
		"completion [command]",
	},
		cmd.Use,
	)
}

// SyncViperPreRunE returns a Cobra run func that synchronizes Viper environment
// flags prefixed with the provided argument.
//
// Thanks to Carolyn Van Slyck: https://github.com/carolynvs/stingoftheviper
func SyncViperPreRunE(prefix string) func(cmd *cobra.Command, args []string) error {
	prefix = strings.ReplaceAll(strings.ToUpper(prefix), "-", "_")
	return func(cmd *cobra.Command, args []string) error {
		if IsBuiltinCommand(cmd) {
			return nil // No-op for builtins
		}

		v := viper.New()
		viper.SetEnvPrefix(prefix)

		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			suffix := strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
			_ = v.BindEnv(f.Name, prefix+"_"+suffix)

			if !f.Changed && v.IsSet(f.Name) {
				val := v.Get(f.Name)
				_ = cmd.Flags().Set(f.Name, fmt.Sprintf("%v", val))
			}
		})

		return nil
	}
}

// CobraRunFunc is the signature of cobra.Command RunFuncs.
type CobraRunFunc func(cmd *cobra.Command, args []string) error

// RunFuncStack chains together a collection of CobraCommandFuncs into one.
func CommandStack(cmdfns ...CobraRunFunc) CobraRunFunc {
	return func(cmd *cobra.Command, args []string) error {
		for _, cmdfn := range cmdfns {
			if err := cmdfn(cmd, args); err != nil {
				return err
			}
		}
		return nil
	}
}

// RegisterZeroLogFlags adds flags for use in with ZeroLogPreRunE:
// - "$PREFIX-level"
// - "$PREFIX-format"
func RegisterZeroLogFlags(flags *pflag.FlagSet, flagPrefix string) {
	flagPrefix = stringz.DefaultEmpty(flagPrefix, "log")
	flags.String(flagPrefix+"-level", "info", `verbosity of logging ("trace", "debug", "info", "warn", "error")`)
	flags.String(flagPrefix+"-format", "auto", `format of logs ("auto", "human", "json")`)
}

// ZeroLogPreRunE returns a Cobra run func that configures the corresponding
// log level from a command.
//
// The required flags can be added to a command by using
// RegisterLoggingPersistentFlags().
func ZeroLogPreRunE(flagPrefix string, prerunLevel zerolog.Level) CobraRunFunc {
	flagPrefix = stringz.DefaultEmpty(flagPrefix, "log")
	return func(cmd *cobra.Command, args []string) error {
		if IsBuiltinCommand(cmd) {
			return nil // No-op for builtins
		}

		format := MustGetString(cmd, flagPrefix+"-format")
		if format == "human" || (format == "auto" && isatty.IsTerminal(os.Stdout.Fd())) {
			log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})
		}

		level := strings.ToLower(MustGetString(cmd, flagPrefix+"-level"))
		switch level {
		case "trace":
			zerolog.SetGlobalLevel(zerolog.TraceLevel)
		case "debug":
			zerolog.SetGlobalLevel(zerolog.DebugLevel)
		case "info":
			zerolog.SetGlobalLevel(zerolog.InfoLevel)
		case "warn":
			zerolog.SetGlobalLevel(zerolog.WarnLevel)
		case "error":
			zerolog.SetGlobalLevel(zerolog.ErrorLevel)
		case "fatal":
			zerolog.SetGlobalLevel(zerolog.FatalLevel)
		case "panic":
			zerolog.SetGlobalLevel(zerolog.PanicLevel)
		default:
			return fmt.Errorf("unknown log level: %s", level)
		}

		log.WithLevel(prerunLevel).Str("new level", level).Msg("set log level")
		return nil
	}
}

// RegisterOpenTelemetryFlags adds the following flags for use with
// OpenTelemetryPreRunE:
// - "$PREFIX-provider"
// - "$PREFIX-jaeger-endpoint"
// - "$PREFIX-jaeger-service-name"
func RegisterOpenTelemetryFlags(flags *pflag.FlagSet, flagPrefix, serviceName string) {
	bi, _ := debug.ReadBuildInfo()
	flagPrefix = stringz.DefaultEmpty(flagPrefix, "otel")
	serviceName = stringz.DefaultEmpty(serviceName, bi.Main.Path)

	flags.String(flagPrefix+"-provider", "none", `opentelemetry provider for tracing ("none", "jaeger")`)
	flags.String(flagPrefix+"-jaeger-endpoint", "http://jaeger:14268/api/traces", "jaeger collector endpoint")
	flags.String(flagPrefix+"-jaeger-service-name", serviceName, "jaeger service name for trace data")
}

// OpenTelemetryPreRunE returns a Cobra run func that configures the
// corresponding otel provider from a command.
//
// The required flags can be added to a command by using
// RegisterOpenTelemetryFlags().
func OpenTelemetryPreRunE(flagPrefix string, prerunLevel zerolog.Level) CobraRunFunc {
	flagPrefix = stringz.DefaultEmpty(flagPrefix, "otel")
	return func(cmd *cobra.Command, args []string) error {
		if IsBuiltinCommand(cmd) {
			return nil // No-op for builtins
		}

		provider := strings.ToLower(MustGetString(cmd, flagPrefix+"-provider"))
		switch provider {
		case "none":
			// Nothing.
		case "jaeger":
			return initJaegerTracer(
				MustGetString(cmd, flagPrefix+"-jaeger-endpoint"),
				MustGetString(cmd, flagPrefix+"-jaeger-service-name"),
			)
		default:
			return fmt.Errorf("unknown tracing provider: %s", provider)
		}

		log.WithLevel(prerunLevel).Str("new provider", provider).Msg("set tracing provider")
		return nil
	}
}

func initJaegerTracer(endpoint, serviceName string) error {
	exp, err := jaeger.New(jaeger.WithCollectorEndpoint(jaeger.WithEndpoint(endpoint)))
	if err != nil {
		return err
	}

	// Configure the global tracer as a batched, always sampling Jaeger exporter.
	otel.SetTracerProvider(trace.NewTracerProvider(
		trace.WithSampler(trace.AlwaysSample()),
		trace.WithSpanProcessor(trace.NewBatchSpanProcessor(exp)),
		trace.WithResource(resource.NewSchemaless(semconv.ServiceNameKey.String(serviceName))),
	))

	// Configure the global tracer to use the W3C method for propagating contexts
	// across services.
	//
	// For low-level details see:
	// https://www.w3.org/TR/trace-context/
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return nil
}

// RegisterGrpcServerFlags adds the following flags for use with
// GrpcServerFromFlags:
// - "$PREFIX-addr"
// - "$PREFIX-tls-cert-path"
// - "$PREFIX-tls-key-path"
// - "$PREFIX-max-conn-age"
func RegisterGrpcServerFlags(flags *pflag.FlagSet, flagPrefix, serviceName, defaultAddr string, defaultEnabled bool) {
	flagPrefix = stringz.DefaultEmpty(flagPrefix, "grpc")
	serviceName = stringz.DefaultEmpty(serviceName, "grpc")
	defaultAddr = stringz.DefaultEmpty(defaultAddr, ":50051")

	flags.String(flagPrefix+"-addr", defaultAddr, "address to listen on to serve "+serviceName)
	flags.String(flagPrefix+"-tls-cert-path", "", "local path to the TLS certificate used to serve "+serviceName)
	flags.String(flagPrefix+"-tls-key-path", "", "local path to the TLS key used to serve "+serviceName)
	flags.Duration(flagPrefix+"-max-conn-age", 30*time.Second, "how long a connection serving "+serviceName+" should be able to live")
	flags.Bool(flagPrefix+"-enabled", defaultEnabled, "enable "+serviceName+" gRPC server")
}

// GrpcServerFromFlags creates an *grpc.Server as configured by the flags from
// RegisterGrpcServerFlags().
func GrpcServerFromFlags(cmd *cobra.Command, flagPrefix string, opts ...grpc.ServerOption) (*grpc.Server, error) {
	flagPrefix = stringz.DefaultEmpty(flagPrefix, "grpc")
	opts = append(opts, grpc.KeepaliveParams(keepalive.ServerParameters{
		MaxConnectionAge: MustGetDuration(cmd, flagPrefix+"-max-conn-age"),
	}))

	certPath := MustGetStringExpanded(cmd, flagPrefix+"-tls-cert-path")
	keyPath := MustGetStringExpanded(cmd, flagPrefix+"-tls-key-path")

	switch {
	case certPath == "" && keyPath == "":
		log.Warn().Str("prefix", flagPrefix).Msg("grpc server serving plaintext")
		return grpc.NewServer(opts...), nil
	case certPath != "" && keyPath != "":
		creds, err := credentials.NewServerTLSFromFile(certPath, keyPath)
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.Creds(creds))
		return grpc.NewServer(opts...), nil
	default:
		return nil, fmt.Errorf(
			"failed to start gRPC server: must provide both --%s-tls-cert-path and --%s-tls-key-path",
			flagPrefix,
			flagPrefix,
		)
	}
}

// GrpcListenFromFlags listens on an gRPC server using the configuration stored
// in the cobra command that was registered with RegisterGrpcServerFlags.
func GrpcListenFromFlags(cmd *cobra.Command, flagPrefix string, srv *grpc.Server) error {
	flagPrefix = stringz.DefaultEmpty(flagPrefix, "grpc")

	if !MustGetBool(cmd, flagPrefix+"-enabled") {
		return nil
	}

	addr := MustGetStringExpanded(cmd, flagPrefix+"-addr")
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on addr for gRPC server: %w", err)
	}

	if err := srv.Serve(l); err != nil {
		return fmt.Errorf("failed to serve gRPC: %w", err)
	}

	return nil
}

// RegisterHttpServerFlags adds the following flags for use with
// HttpServerFromFlags:
// - "$PREFIX-addr"
// - "$PREFIX-tls-cert-path"
// - "$PREFIX-tls-key-path"
// - "$PREFIX-enabled"
func RegisterHttpServerFlags(flags *pflag.FlagSet, flagPrefix, serviceName, defaultAddr string, defaultEnabled bool) {
	flagPrefix = stringz.DefaultEmpty(flagPrefix, "http")
	serviceName = stringz.DefaultEmpty(serviceName, "http")
	defaultAddr = stringz.DefaultEmpty(defaultAddr, ":8443")

	flags.String(flagPrefix+"-addr", defaultAddr, "address to listen on to serve "+serviceName)
	flags.String(flagPrefix+"-tls-cert-path", "", "local path to the TLS certificate used to serve "+serviceName)
	flags.String(flagPrefix+"-tls-key-path", "", "local path to the TLS key used to serve "+serviceName)
	flags.Bool(flagPrefix+"-enabled", defaultEnabled, "enable "+serviceName+" http server")
}

// HttpServerFromFlags creates an *http.Server as configured by the flags from
// RegisterGrpcServerFlags().
func HttpServerFromFlags(cmd *cobra.Command, flagPrefix string) *http.Server {
	flagPrefix = stringz.DefaultEmpty(flagPrefix, "http")
	return &http.Server{
		Addr: MustGetStringExpanded(cmd, flagPrefix+"-addr"),
	}
}

// HttpListenFromFlags listens on an HTTP server using the configuration stored
// in the cobra command that was registered with RegisterHttpServerFlags.
func HttpListenFromFlags(cmd *cobra.Command, flagPrefix string, srv *http.Server) error {
	if !MustGetBool(cmd, flagPrefix+"-enabled") {
		return nil
	}

	certPath := MustGetStringExpanded(cmd, flagPrefix+"-tls-cert-path")
	keyPath := MustGetStringExpanded(cmd, flagPrefix+"-tls-key-path")

	switch {
	case certPath == "" && keyPath == "":
		log.Warn().Str("prefix", flagPrefix).Msg("http server serving plaintext")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("failed while serving http: %w", err)
		}
		return nil
	case certPath != "" && keyPath != "":
		if err := srv.ListenAndServeTLS(certPath, keyPath); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("failed while serving https: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("failed to start http server: must provide both --%s-tls-cert-path and --%s-tls-key-path",
			flagPrefix,
			flagPrefix,
		)
	}
}
