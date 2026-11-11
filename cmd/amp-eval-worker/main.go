// Command amp-eval-worker runs the evaluation module's workloads outside the
// core binary:
//
//	amp-eval-worker controller    --core URL --credential C [--runner process|kubernetes]
//	amp-eval-worker shared-worker --core URL --credential C
//	amp-eval-worker launcher      --core URL --run ID        (token in AMP_RUN_TOKEN)
//
// controller and shared-worker are long-running services (two Deployments).
// launcher is the entry point of one isolated job, started by the controller.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/a5anka/amp-modular-poc/modules/evaluation/evalproto"
	"github.com/a5anka/amp-modular-poc/modules/evaluation/worker"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	core := fs.String("core", envOr("AMP_CORE_URL", "http://localhost:8080"), "core URL")
	cred := fs.String("credential", os.Getenv("AMP_WORKLOAD_CREDENTIAL"), "workload credential (controller, shared-worker)")
	instance := fs.String("instance", os.Getenv("HOSTNAME"), "instance name reported in heartbeats")
	runner := fs.String("runner", envOr("AMP_EVAL_RUNNER", "process"), "controller: process | kubernetes")
	capacity := fs.Int("capacity", 4, "controller: isolated runs in flight")
	ns := fs.String("namespace", envOr("POD_NAMESPACE", "amp-eval"), "kubernetes runner: namespace for run Jobs")
	image := fs.String("image", envOr("AMP_EVAL_IMAGE", "ghcr.io/a5anka/amp-eval-worker:0.2.0"), "kubernetes runner: launcher image")
	runtimeClass := fs.String("runtime-class", "gvisor", "kubernetes runner: RuntimeClass for run pods")
	dryRun := fs.Bool("dry-run", false, "kubernetes runner: log Job manifests instead of creating them")
	runID := fs.String("run", "", "launcher: run ID")
	_ = fs.Parse(os.Args[2:])

	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("workload", cmd)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "controller":
		need(*cred, "--credential (or AMP_WORKLOAD_CREDENTIAL)")
		var r worker.Runner
		switch *runner {
		case "process":
			self, err := os.Executable()
			if err != nil {
				fail(err)
			}
			r = &worker.ProcessRunner{Launcher: self}
		case "kubernetes":
			r = &worker.KubernetesRunner{Namespace: *ns, Image: *image, RuntimeClass: *runtimeClass, DryRun: *dryRun, Log: log}
		default:
			fail(fmt.Errorf("unknown runner %q", *runner))
		}
		log.Info("starting", "core", *core, "runner", r.Name(), "protocol", evalproto.Protocol)
		(&worker.Controller{Session: &worker.Session{Core: *core, Credential: *cred, Instance: *instance, Log: log},
			Runner: r, Capacity: *capacity, Log: log}).Run(ctx)
	case "shared-worker":
		need(*cred, "--credential (or AMP_WORKLOAD_CREDENTIAL)")
		log.Info("starting", "core", *core, "protocol", evalproto.Protocol)
		(&worker.SharedWorker{Session: &worker.Session{Core: *core, Credential: *cred, Instance: *instance, Log: log}, Log: log}).Run(ctx)
	case "launcher":
		need(*runID, "--run")
		tok := os.Getenv("AMP_RUN_TOKEN")
		need(tok, "AMP_RUN_TOKEN")
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if err := worker.Launch(ctx, *core, *runID, tok, log); err != nil {
			fail(err)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: amp-eval-worker controller|shared-worker|launcher [flags]")
	os.Exit(2)
}

func need(v, what string) {
	if v == "" {
		fail(fmt.Errorf("missing %s", what))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
