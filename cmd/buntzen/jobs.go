package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/config"
	"github.com/jaysqvl/buntzen-pass-bot/internal/control"
	"github.com/jaysqvl/buntzen-pass-bot/internal/engine"
	"github.com/jaysqvl/buntzen-pass-bot/internal/lockfile"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/store"
)

func runJobCommand(parent context.Context, cfg config.Config, database *store.Store, commandName string, args []string) error {
	flags := flag.NewFlagSet(commandName, flag.ContinueOnError)
	bookingID := flags.Int64("booking", 0, "booking request ID")
	mode := flags.String("mode", "", "manual or auto override for book")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *bookingID <= 0 {
		return errors.New(commandName + " requires --booking ID")
	}
	command := model.JobCommand(commandName)
	runMode := model.RunMode("")
	requestedMode := strings.TrimSpace(*mode)
	if command == model.CommandDryRun {
		if requestedMode != "" {
			return errors.New("--mode is only valid for book")
		}
		runMode = model.RunModeDryRun
	} else if command == model.CommandBook && requestedMode != "" {
		runMode = model.RunMode(requestedMode)
		if runMode != model.RunModeManual && runMode != model.RunModeAuto {
			return errors.New("book --mode must be manual or auto")
		}
	} else if command != model.CommandBook && requestedMode != "" {
		return errors.New("--mode is only valid for book")
	}
	instanceLock, lockErr := lockfile.TryAcquire(cfg.AppDataDir + "/control-plane.lock")
	ownsControlPlane := lockErr == nil
	if lockErr != nil && !errors.Is(lockErr, lockfile.ErrLocked) {
		return lockErr
	}
	if !ownsControlPlane && !servingControlPlaneHealthy(cfg) {
		return errors.New("another one-shot command owns this appdata directory; wait for it to finish before queueing another job")
	}
	if ownsControlPlane {
		defer instanceLock.Close()
		if command == model.CommandBook && runMode != model.RunModeAuto {
			booking, err := database.SystemGetBookingRequest(parent, *bookingID)
			if err != nil {
				return err
			}
			if runMode == "" {
				runMode = booking.ConfirmationMode
			}
			if runMode == model.RunModeManual {
				return errors.New("manual book requires the running web control plane for approval; start serve or use --mode auto")
			}
		}
		if _, err := database.SystemRecoverInterruptedJobs(parent); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	jobEngine := engine.New(cfg, database, control.NewHub())
	if ownsControlPlane {
		jobEngine.Start(ctx)
		defer jobEngine.Stop()
	}
	job, err := jobEngine.SystemQueueBooking(ctx, *bookingID, command, runMode)
	if err != nil {
		return err
	}
	fmt.Printf("queued job %d\n", job.ID)
	slog.Info("one-shot command queued job", "job_id", job.ID, "command", command, "mode", runMode)
	finished, err := jobEngine.SystemWait(ctx, job.ID)
	if err != nil {
		if ctx.Err() != nil {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = database.SystemRequestJobCancellation(cancelCtx, job.ID)
			cancel()
			return errors.New("interrupted; durable job cancellation was requested")
		}
		return err
	}
	fmt.Printf("job %d finished: %s\n", finished.ID, finished.Status)
	slog.Info("one-shot command finished", "job_id", finished.ID, "status", finished.Status)
	if finished.Status != model.JobSucceeded {
		return fmt.Errorf("job %d did not succeed: %s", finished.ID, finished.Message)
	}
	return nil
}

func servingControlPlaneHealthy(cfg config.Config) bool {
	address := cfg.ListenAddress
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	target := "http://" + net.JoinHostPort(host, port) + "/healthz"
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(target)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}
