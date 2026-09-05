//go:build integration

package integration_test

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaysqvl/buntzen-pass-bot/internal/actionproc"
	"github.com/jaysqvl/buntzen-pass-bot/internal/control"
	"github.com/jaysqvl/buntzen-pass-bot/internal/model"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp"
	"github.com/jaysqvl/buntzen-pass-bot/internal/otp/bluebubbles"
)

const (
	bookingTargetDate = "2030-01-06"
	bookingVehicle    = "Synthetic vehicle"
	// This is an unsigned, synthetic JWT with only a far-future exp claim. The
	// fake Yodel page installs it before tracing starts so the real worker takes
	// the already-authenticated booking path without any private credentials.
	bookingBearerToken = "eyJhbGciOiJub25lIn0.eyJleHAiOjQxNDI0NDQ4MDB9."
)

// TestControlPlanePythonBrowserBooking exercises the booking half of the real
// Go -> JSONL -> Python -> Playwright path against process-local HTTPS. It is
// intentionally separate from the OTP integration so a booking regression is
// distinguishable from a login/provider failure.
func TestControlPlanePythonBrowserBooking(t *testing.T) {
	if testing.Short() {
		t.Skip("real-browser integration test")
	}

	cases := []struct {
		name     string
		jobID    int64
		command  model.JobCommand
		mode     model.RunMode
		decision model.ApprovalDecision
		status   model.JobStatus
		receipt  string
	}{
		{name: "dry-run stops before cart", jobID: 9101, command: model.CommandDryRun, mode: model.RunModeDryRun, status: model.JobSucceeded},
		{name: "manual approval confirms once", jobID: 9102, command: model.CommandBook, mode: model.RunModeManual, decision: model.DecisionApprove, status: model.JobSucceeded},
		{name: "manual cancellation never confirms", jobID: 9103, command: model.CommandBook, mode: model.RunModeManual, decision: model.DecisionCancel, status: model.JobCancelled},
		{name: "automatic booking confirms once", jobID: 9104, command: model.CommandBook, mode: model.RunModeAuto, status: model.JobSucceeded},
		{name: "sold out after click is unknown", jobID: 9105, command: model.CommandBook, mode: model.RunModeAuto, status: model.JobOutcomeUnknown, receipt: "sold_out"},
		{name: "success dialog without an issued pass is unknown", jobID: 9106, command: model.CommandBook, mode: model.RunModeAuto, status: model.JobOutcomeUnknown, receipt: "empty_wallet"},
		{name: "slow receipt body is awaited", jobID: 9107, command: model.CommandBook, mode: model.RunModeAuto, status: model.JobSucceeded, receipt: "slow_body"},
		{name: "leftover cart is never submitted", jobID: 9108, command: model.CommandBook, mode: model.RunModeAuto, status: model.JobFailed, receipt: "stale_cart"},
		{name: "extra cart quantity is never submitted", jobID: 9109, command: model.CommandBook, mode: model.RunModeAuto, status: model.JobFailed, receipt: "extra_cart"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			outcome := runSyntheticBrowserBooking(t, testCase.jobID, testCase.command, testCase.mode, testCase.decision, testCase.receipt)
			if outcome.err != nil {
				t.Fatalf("run coordinated booking: %v\nworker stderr:\n%s", outcome.err, strings.Join(outcome.stderr, "\n"))
			}
			if outcome.result.Status != testCase.status {
				t.Fatalf("booking status = %q, want %q; result=%#v\nworker stderr:\n%s", outcome.result.Status, testCase.status, outcome.result, strings.Join(outcome.stderr, "\n"))
			}
			if testCase.status == model.JobSucceeded && outcome.result.ExitCode != 0 {
				t.Errorf("successful booking exit code = %d, want 0", outcome.result.ExitCode)
			}

			snapshot := outcome.flow
			if len(snapshot.errors) != 0 {
				t.Fatalf("fake Yodel contract violations: %s", strings.Join(snapshot.errors, "; "))
			}
			if snapshot.probeLoads != 1 || snapshot.passLoads != 1 {
				t.Errorf("authenticated probe/pass loads = %d/%d, want 1/1", snapshot.probeLoads, snapshot.passLoads)
			}
			if snapshot.dateSelections != 1 || snapshot.vehicleSelections != 1 {
				t.Errorf("date/vehicle selections = %d/%d, want 1/1", snapshot.dateSelections, snapshot.vehicleSelections)
			}
			if outcome.blueBubblesCalls != 0 {
				t.Errorf("already-authenticated booking touched BlueBubbles %d times", outcome.blueBubblesCalls)
			}

			switch {
			case testCase.status == model.JobFailed:
				wantAdds := 1
				if testCase.receipt == "stale_cart" {
					wantAdds = 0
				}
				if snapshot.cartAdds != wantAdds || snapshot.checkouts != 0 || snapshot.confirmations != 0 || outcome.confirmationStarts != 0 {
					t.Errorf("unsafe cart advanced: adds=%d checkout=%d confirmations=%d barriers=%d", snapshot.cartAdds, snapshot.checkouts, snapshot.confirmations, outcome.confirmationStarts)
				}
				assertEventKindsAbsent(t, outcome.events, "confirmation.starting", "confirmation.completed")
			case testCase.status == model.JobOutcomeUnknown:
				assertCheckoutCounts(t, snapshot, 1)
				if outcome.confirmationStarts != 1 {
					t.Errorf("unknown checkout had %d confirmation barriers, want 1", outcome.confirmationStarts)
				}
				assertEventKinds(t, outcome.events, "confirmation.starting")
				assertEventKindsAbsent(t, outcome.events, "confirmation.completed")
			case testCase.command == model.CommandDryRun:
				if snapshot.cartAdds != 0 || snapshot.checkouts != 0 || snapshot.confirmations != 0 {
					t.Errorf("dry-run crossed purchase boundary: cart=%d checkout=%d final=%d", snapshot.cartAdds, snapshot.checkouts, snapshot.confirmations)
				}
				if outcome.approvalRequests != 0 || outcome.confirmationStarts != 0 {
					t.Errorf("dry-run requested approval/final barrier: approval=%d confirmation=%d", outcome.approvalRequests, outcome.confirmationStarts)
				}
				assertEventKinds(t, outcome.events, "run.checking_pass")
			case testCase.mode == model.RunModeManual && testCase.decision == model.DecisionApprove:
				assertCheckoutCounts(t, snapshot, 1)
				if outcome.beforeDecision.confirmations != 0 {
					t.Error("manual booking clicked final confirmation before operator approval")
				}
				if outcome.approvalRequests != 1 || outcome.confirmationStarts != 1 {
					t.Errorf("manual approve lifecycle: approval=%d confirmation=%d, want 1/1", outcome.approvalRequests, outcome.confirmationStarts)
				}
				if !errors.Is(outcome.secondDecisionErr, control.ErrDecisionAlreadySet) && !errors.Is(outcome.secondDecisionErr, control.ErrDecisionNotPending) {
					t.Errorf("opposite decision after approval was not rejected: %v", outcome.secondDecisionErr)
				}
				assertEventKinds(t, outcome.events, "approval.requested", "approval.approved", "confirmation.starting", "confirmation.completed")
			case testCase.mode == model.RunModeManual && testCase.decision == model.DecisionCancel:
				if snapshot.cartAdds != 1 || snapshot.checkouts != 1 || snapshot.confirmations != 0 {
					t.Errorf("manual cancel purchase counts: cart=%d checkout=%d final=%d, want 1/1/0", snapshot.cartAdds, snapshot.checkouts, snapshot.confirmations)
				}
				if outcome.beforeDecision.confirmations != 0 {
					t.Error("manual booking clicked final confirmation before operator cancellation")
				}
				if outcome.approvalRequests != 1 || outcome.confirmationStarts != 0 {
					t.Errorf("manual cancel lifecycle: approval=%d confirmation=%d, want 1/0", outcome.approvalRequests, outcome.confirmationStarts)
				}
				if !errors.Is(outcome.secondDecisionErr, control.ErrDecisionAlreadySet) && !errors.Is(outcome.secondDecisionErr, control.ErrDecisionNotPending) {
					t.Errorf("opposite decision after cancellation was not rejected: %v", outcome.secondDecisionErr)
				}
				assertEventKinds(t, outcome.events, "approval.requested", "approval.cancelled")
				assertEventKindsAbsent(t, outcome.events, "confirmation.starting", "confirmation.completed")
			case testCase.mode == model.RunModeAuto:
				assertCheckoutCounts(t, snapshot, 1)
				if outcome.approvalRequests != 0 || outcome.confirmationStarts != 1 {
					t.Errorf("automatic lifecycle: approval=%d confirmation=%d, want 0/1", outcome.approvalRequests, outcome.confirmationStarts)
				}
				assertEventKinds(t, outcome.events, "confirmation.starting", "confirmation.completed")
				assertEventKindsAbsent(t, outcome.events, "approval.requested")
			}

			observed := strings.Join(append(append([]string{}, outcome.stderr...), outcome.events...), "\n")
			assertExcludesSecrets(t, []byte(observed), "booking worker stderr and durable events")
			assertExcludesValue(t, []byte(observed), bookingBearerToken, "booking worker stderr and durable events")
			assertTreeExcludesSecrets(t, outcome.artifactDir)
			assertTreeExcludesValue(t, outcome.artifactDir, bookingBearerToken)
		})
	}
}

type bookingRunOutcome struct {
	result             control.RunResult
	err                error
	flow               bookingFlowSnapshot
	beforeDecision     bookingFlowSnapshot
	stderr             []string
	events             []string
	artifactDir        string
	blueBubblesCalls   int32
	approvalRequests   int32
	confirmationStarts int32
	secondDecisionErr  error
}

func runSyntheticBrowserBooking(t *testing.T, jobID int64, command model.JobCommand, mode model.RunMode, decision model.ApprovalDecision, receipt string) bookingRunOutcome {
	t.Helper()
	repoRoot := repositoryRoot(t)
	python, pythonArgs := pythonWorker(t, repoRoot)
	var confirmationBarrier atomic.Bool
	flow := &bookingFlow{confirmationBarrier: &confirmationBarrier, receipt: receipt}
	yodel := httptest.NewUnstartedServer(http.HandlerFunc(flow.serveYodel))
	yodel.Config.ErrorLog = log.New(io.Discard, "", 0)
	yodel.StartTLS()
	t.Cleanup(yodel.Close)

	var blueBubblesCalls atomic.Int32
	blueBubbles := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		blueBubblesCalls.Add(1)
		http.Error(response, "OTP provider must not be called for an authenticated session", http.StatusInternalServerError)
	}))
	t.Cleanup(blueBubbles.Close)
	provider, err := bluebubbles.New(bluebubbles.Config{
		BaseURL:  blueBubbles.URL,
		Password: testBBPassword,
		ChatGUID: testChatGUID,
		Sender:   testSender,
		Service:  testService,
	})
	if err != nil {
		t.Fatalf("create synthetic BlueBubbles provider: %v", err)
	}

	profileDir := filepath.Join(t.TempDir(), "browser-profile")
	artifactDir := filepath.Join(t.TempDir(), "artifacts")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	hub := control.NewHub()
	jobKey := fmt.Sprint(jobID)

	var outputMu sync.Mutex
	var stderrLines []string
	var durableEvents []string
	var approvalRequests atomic.Int32
	var confirmationStarts atomic.Int32
	approvalReady := make(chan struct{}, 1)
	input := control.RunInput{
		JobID:   jobID,
		Command: command,
		Mode:    mode,
		StartConfig: map[string]any{
			"profile_dir":           profileDir,
			"target_date":           bookingTargetDate,
			"timezone":              "America/Vancouver",
			"login_probe_url":       yodel.URL + "/buntzen-lake",
			"allowed_yodel_origins": []string{yodel.URL},
			"all_day_pass_url":      yodel.URL + "/buntzen-lake/All-Day-Pass",
			"half_day_pass_url":     nil,
			"vehicle_keyword":       bookingVehicle,
			"pass_order":            []string{"all_day"},
			"headless":              true,
			"browser_channel":       nil,
			"executable_path":       nullableString(browserPath()),
			"default_timeout_ms":    8_000,
			"poll_deadline_seconds": 2,
			"poll_min_seconds":      0.05,
			"poll_max_seconds":      0.1,
			"artifacts_dir":         artifactDir,
		},
		Credentials: model.ProfileCredentials{},
		Provider:    provider,
		OTPFilter: otp.Filter{
			ChatGUID: testChatGUID,
			Sender:   testSender,
			Service:  testService,
		},
		OTPTimeout:  5 * time.Second,
		CancelGrace: 5 * time.Second,
		Hub:         hub,
		NewProcess: func(processCtx context.Context) (control.ActionProcess, error) {
			return actionproc.Start(processCtx, actionproc.Config{
				Executable: python,
				Args:       pythonArgs,
				Environment: []string{
					"BUNTZEN_ACTIONPROC_HELPER=e2e-local-tls",
					"BUNTZEN_ACTION_LOG_LEVEL=debug",
					"PYTHONDONTWRITEBYTECODE=1",
					"PYTHONUNBUFFERED=1",
				},
				CancelGrace: 5 * time.Second,
				OnStderr: func(line string) {
					outputMu.Lock()
					stderrLines = append(stderrLines, line)
					outputMu.Unlock()
				},
			})
		},
		Hooks: control.RunHooks{
			Event: func(kind, message string) {
				outputMu.Lock()
				durableEvents = append(durableEvents, kind+":"+message)
				outputMu.Unlock()
			},
			AwaitingApproval: func(string) error {
				approvalRequests.Add(1)
				approvalReady <- struct{}{}
				return nil
			},
			ConfirmationStarting: func() error {
				confirmationStarts.Add(1)
				confirmationBarrier.Store(true)
				return nil
			},
		},
	}

	var result control.RunResult
	var runErr error
	var beforeDecision bookingFlowSnapshot
	var secondDecisionErr error
	if mode == model.RunModeManual {
		done := make(chan struct{})
		go func() {
			result, runErr = control.Run(ctx, input)
			close(done)
		}()
		select {
		case <-approvalReady:
			beforeDecision = flow.snapshot()
			if err := hub.Decide(jobKey, string(decision)); err != nil {
				t.Fatalf("submit first manual decision: %v", err)
			}
			opposite := model.DecisionCancel
			if decision == model.DecisionCancel {
				opposite = model.DecisionApprove
			}
			secondDecisionErr = hub.Decide(jobKey, string(opposite))
		case <-done:
			outputMu.Lock()
			workerOutput := strings.Join(stderrLines, "\n")
			outputMu.Unlock()
			t.Fatalf("browser exited before manual approval: result=%#v error=%v\nworker stderr:\n%s", result, runErr, workerOutput)
		case <-ctx.Done():
			outputMu.Lock()
			workerOutput := strings.Join(stderrLines, "\n")
			outputMu.Unlock()
			t.Fatalf("browser did not reach manual approval: %v; flow=%#v\nworker stderr:\n%s", ctx.Err(), flow.snapshot(), workerOutput)
		}
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatalf("manual browser run did not finish: %v", ctx.Err())
		}
	} else {
		result, runErr = control.Run(ctx, input)
	}

	outputMu.Lock()
	stderrCopy := append([]string(nil), stderrLines...)
	eventCopy := append([]string(nil), durableEvents...)
	outputMu.Unlock()
	return bookingRunOutcome{
		result:             result,
		err:                runErr,
		flow:               flow.snapshot(),
		beforeDecision:     beforeDecision,
		stderr:             stderrCopy,
		events:             eventCopy,
		artifactDir:        artifactDir,
		blueBubblesCalls:   blueBubblesCalls.Load(),
		approvalRequests:   approvalRequests.Load(),
		confirmationStarts: confirmationStarts.Load(),
		secondDecisionErr:  secondDecisionErr,
	}
}

type bookingFlow struct {
	mu sync.Mutex

	confirmationBarrier *atomic.Bool
	receipt             string
	probeLoads          int
	passLoads           int
	dateSelections      int
	vehicleSelections   int
	cartAdds            int
	checkouts           int
	confirmations       int
	errors              []string
}

type bookingFlowSnapshot struct {
	probeLoads        int
	passLoads         int
	dateSelections    int
	vehicleSelections int
	cartAdds          int
	checkouts         int
	confirmations     int
	errors            []string
}

func (f *bookingFlow) snapshot() bookingFlowSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return bookingFlowSnapshot{
		probeLoads:        f.probeLoads,
		passLoads:         f.passLoads,
		dateSelections:    f.dateSelections,
		vehicleSelections: f.vehicleSelections,
		cartAdds:          f.cartAdds,
		checkouts:         f.checkouts,
		confirmations:     f.confirmations,
		errors:            append([]string(nil), f.errors...),
	}
}

func (f *bookingFlow) serveYodel(response http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/buntzen-lake":
		f.probeLoads++
		writeHTML(response, `<html><body><script>localStorage.setItem("BearerToken", "`+bookingBearerToken+`");</script><a href="/account">My Account</a></body></html>`)
	case request.Method == http.MethodGet && request.URL.Path == "/buntzen-lake/All-Day-Pass":
		f.passLoads++
		page := bookingPassPage
		if f.receipt == "stale_cart" {
			page = strings.Replace(page, emptyBookingCart, singleBookingCart, 1)
		}
		writeHTML(response, page)
	case request.Method == http.MethodPost && request.URL.Path == "/synthetic/date-selected":
		f.dateSelections++
		response.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodPost && request.URL.Path == "/synthetic/vehicle-selected":
		f.vehicleSelections++
		response.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodPost && request.URL.Path == "/cart":
		f.cartAdds++
		if err := request.ParseForm(); err != nil {
			f.errors = append(f.errors, "parse cart form: "+err.Error())
		}
		if request.Form.Get("target_date") != bookingTargetDate || request.Form.Get("vehicle") != bookingVehicle || request.Form.Get("pass") != "all_day" {
			f.errors = append(f.errors, "cart did not retain selected date, vehicle, and pass")
		}
		cart := singleBookingCart
		if f.receipt == "extra_cart" {
			cart = strings.ReplaceAll(cart, `value="1"`, `value="2"`)
		}
		writeHTML(response, `<html><body>`+cart+`<form method="post" action="/checkout"><button id="checkOutButton" type="submit">Checkout</button></form></body></html>`)
	case request.Method == http.MethodPost && request.URL.Path == "/checkout":
		f.checkouts++
		writeHTML(response, `<html><body>`+singleBookingCart+`
<div id="orderConfirmModel"><button onclick="submitOrder()">Yes</button></div>
<div id="orderConfirmModal" style="display:none"><h2 class="heading">Confirmed</h2><a href="/wallet">See My Pass</a></div>
<div id="orderErrorModal" style="display:none">Sorry, sold out. Booking failed.</div>
<script>
async function submitOrder() {
  const result = await fetch('/api/orders/checkout', {method: 'POST'});
  const order = await result.json();
  document.getElementById('orderConfirmModel').style.display = 'none';
  setTimeout(() => {
    const id = order.payment.succeeded ? 'orderConfirmModal' : 'orderErrorModal';
    document.getElementById(id).style.display = 'block';
  }, 250);
}
</script></body></html>`)
	case request.Method == http.MethodPost && request.URL.Path == "/api/orders/checkout":
		f.confirmations++
		if f.confirmationBarrier == nil || !f.confirmationBarrier.Load() {
			f.errors = append(f.errors, "final confirmation arrived before the durable control-plane barrier")
		}
		items := []any{map[string]any{"summaryField1": map[string]any{"value": "Synthetic pass"}}}
		if f.receipt == "empty_wallet" {
			items = []any{}
		}
		response.Header().Set("Content-Type", "application/json")
		if f.receipt == "slow_body" {
			response.WriteHeader(http.StatusOK)
			response.(http.Flusher).Flush()
			time.Sleep(time.Second)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"payment":     map[string]any{"succeeded": f.receipt != "sold_out", "orderId": 123, "errorMessage": nil},
			"walletItems": items,
		})
	default:
		f.errors = append(f.errors, fmt.Sprintf("unexpected fake Yodel request %s %s", request.Method, request.URL.Path))
		http.NotFound(response, request)
	}
}

const emptyBookingCart = `<div class="shoppingCard inactive"><div class="cartDigit"><div class="counter"><span class="count">0</span></div></div></div>`

const singleBookingCart = `<div class="shoppingCard inactive"><div class="cartDigit"><div class="counter"><span class="count">1</span></div></div>
<div class="shoppingMainList"><ul><li class="shoppingList singleItemList"><div class="CardListing"><div class="ClassificationInnerRow"><input class="count" value="1"></div></div></li></ul></div></div>`

const bookingPassPage = `<!doctype html>
<html>
  <body>` + emptyBookingCart + `
    <script>
      function recordSelection(path) {
        const request = new XMLHttpRequest();
        request.open("POST", path, false);
        request.send();
      }
    </script>
    <div class="card ImageCard">
      <h2>All-day pass</h2>
      <div class="dateMain">
        <div class="dateHeader"><span class="month">January-2030</span></div>
        <div class="datelist">
          <button class="date active" type="button" aria-label="Saturday 05">05</button>
          <button class="date" type="button" aria-label="Sunday 06"
            onclick="this.previousElementSibling.classList.remove('active'); this.classList.add('active'); document.getElementById('target-date').value='2030-01-06'; recordSelection('/synthetic/date-selected')">06</button>
        </div>
      </div>
      <a class="smartSelectCustom" href="#" onclick="document.getElementById('vehicle-popup').style.display='block'; return false">Choose a vehicle</a>
      <div id="vehicle-popup" class="popup smart-select-popup" style="display:none">
        <label class="item-radio" onclick="document.getElementById('vehicle').value='Synthetic vehicle'; recordSelection('/synthetic/vehicle-selected')">
          <input type="radio" name="vehicle-choice"><span class="item-title">Synthetic vehicle</span>
        </label>
        <a class="link popup-close" href="#" onclick="document.getElementById('vehicle-popup').style.display='none'; return false">Done</a>
      </div>
      <form method="post" action="/cart">
        <input id="target-date" type="hidden" name="target_date">
        <input id="vehicle" type="hidden" name="vehicle">
        <input type="hidden" name="pass" value="all_day">
        <a href="#" onclick="this.closest('form').requestSubmit(); return false">Add To Cart</a>
      </form>
    </div>
  </body>
</html>`

func assertCheckoutCounts(t *testing.T, snapshot bookingFlowSnapshot, confirmations int) {
	t.Helper()
	if snapshot.cartAdds != 1 || snapshot.checkouts != 1 || snapshot.confirmations != confirmations {
		t.Errorf("purchase counts: cart=%d checkout=%d final=%d, want 1/1/%d", snapshot.cartAdds, snapshot.checkouts, snapshot.confirmations, confirmations)
	}
}

func assertEventKinds(t *testing.T, events []string, expected ...string) {
	t.Helper()
	joined := strings.Join(events, "\n")
	for _, kind := range expected {
		if !strings.Contains(joined, kind+":") {
			t.Errorf("durable event stream did not contain %s; events:\n%s", kind, joined)
		}
	}
}

func assertEventKindsAbsent(t *testing.T, events []string, forbidden ...string) {
	t.Helper()
	joined := strings.Join(events, "\n")
	for _, kind := range forbidden {
		if strings.Contains(joined, kind+":") {
			t.Errorf("durable event stream unexpectedly contained %s; events:\n%s", kind, joined)
		}
	}
}

func assertTreeExcludesValue(t *testing.T, root, forbidden string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".zip") {
			archive, err := zip.OpenReader(path)
			if err != nil {
				return err
			}
			defer archive.Close()
			for _, item := range archive.File {
				reader, err := item.Open()
				if err != nil {
					return err
				}
				contents, err := io.ReadAll(io.LimitReader(reader, 16<<20))
				_ = reader.Close()
				if err != nil {
					return err
				}
				assertExcludesValue(t, contents, forbidden, filepath.Base(path)+":"+item.Name)
			}
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assertExcludesValue(t, contents, forbidden, filepath.Base(path))
		return nil
	})
	if err != nil {
		t.Fatalf("inspect booking integration artifacts: %v", err)
	}
}

func assertExcludesValue(t *testing.T, contents []byte, forbidden, label string) {
	t.Helper()
	if strings.Contains(string(contents), forbidden) {
		t.Errorf("%s retained a synthetic bearer credential", label)
	}
}
