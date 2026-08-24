from __future__ import annotations

import logging
import random
import re
import time
from dataclasses import dataclass
from datetime import datetime
from typing import Any, Iterable, Optional

from .config import ActionConfig
from .control import ControlPort
from .diagnostics import SafeDiagnostics
from .errors import ActionError, OutcomeUnknown
from .pass_types import PassPreference, build_pass_order


logger = logging.getLogger("buntzen_actions.yodel")


AUTHENTICATED_SELECTORS = (
    ".datelist button.date",
    "button.date",
    ".card.ImageCard",
    "#checkOutButton",
    "text=Logout",
    "text=My Account",
    "text=Vehicles",
)
LOGIN_EMAIL_SELECTORS = (
    "input[type='email']",
    "input[name*='email' i]",
    "input[autocomplete='username']",
    "input[placeholder*='email' i]",
    "input[placeholder*='phone' i]",
)
LOGIN_PASSWORD_SELECTORS = (
    "input[type='password']",
    "input[name*='password' i]",
    "input[autocomplete='current-password']",
)
LOGIN_SUBMIT_SELECTORS = (
    "button:has-text('Log in')",
    "button:has-text('Login')",
    "button:has-text('Sign in')",
    "button:has-text('Continue')",
    "a:has-text('Log in')",
    "a:has-text('Sign in')",
)
OTP_INPUT_SELECTORS = (
    "input[autocomplete='one-time-code']",
    "input[name*='otp' i]",
    "input[name*='code' i]",
    "input[placeholder*='code' i]",
    "input[inputmode='numeric']",
)
OTP_REQUEST_SELECTORS = (
    "button:has-text('Send code')",
    "button:has-text('Resend code')",
    "button:has-text('Send verification')",
    "button:has-text('Text me')",
    "a:has-text('Send code')",
    "a:has-text('Resend code')",
)
OTP_SUBMIT_SELECTORS = (
    "button:has-text('Verify')",
    "button:has-text('Submit')",
    "button:has-text('Continue')",
    "button:has-text('Confirm')",
    "a:has-text('Verify')",
    "a:has-text('Continue')",
)
VEHICLE_SELECTOR_SELECTORS = (
    ".smartSelectCustom",
    "text=Select Vehicle",
    "text=Vehicle",
)
ADD_TO_CART_SELECTORS = (
    "a:has-text('Add To Cart')",
    "button:has-text('Add To Cart')",
    "a:has-text('Add to Cart')",
    "button:has-text('Add to Cart')",
)
CHECKOUT_SELECTORS = (
    "#checkOutButton",
    "button:has-text('Checkout')",
    "a:has-text('Checkout')",
    "button:has-text('Check out')",
    "a:has-text('Check out')",
)
FINAL_CONFIRM_SELECTORS = (
    "a:has-text('Yes')",
    "button:has-text('Yes')",
    "button:has-text('Confirm')",
    "a:has-text('Confirm')",
)
DATE_BUTTON_SELECTORS = (
    ".datelist button.date",
    "button.date",
    "button[aria-label*='{day}']",
    "button:has-text('{day}')",
)


@dataclass(frozen=True)
class BookingResult:
    success: bool
    message: str
    pass_key: Optional[str] = None


class YodelAction:
    """The single allowlisted browser action exposed by this worker."""

    def __init__(
        self,
        page: Any,
        context: Any,
        config: ActionConfig,
        control: ControlPort,
        diagnostics: SafeDiagnostics,
    ) -> None:
        self.page = page
        self.context = context
        self.config = config
        self.control = control
        self.diagnostics = diagnostics
        # Keep third-party subresources available to the real Yodel site, but
        # fail closed on any top-level redirect or scripted navigation away
        # from the operator-approved credential origin.
        self.page.route("**/*", self._guard_navigation)

    def execute(self) -> BookingResult:
        if self.config.command == "auth-check":
            if not self.ensure_authenticated():
                return BookingResult(False, "Yodel authentication did not complete.")
            return BookingResult(True, "Yodel session is authenticated.")

        auth_deadline_at = (
            self.config.auth_deadline_at if self.config.command == "book" else None
        )
        if not self.ensure_authenticated(auth_deadline_at=auth_deadline_at):
            return BookingResult(False, "Yodel authentication did not complete.")

        if self.config.command == "dry-run":
            return self.try_booking_once(mode="dry-run")

        if self.config.command != "book":
            raise ActionError("Unsupported Yodel action command")
        self.wait_for_release_if_needed()
        return self.poll_for_booking(mode=self.config.mode)

    def ensure_authenticated(self, auth_deadline_at: Optional[datetime] = None) -> bool:
        self.diagnostics.pause_for_auth()
        self.control.status("auth", "Checking Yodel authentication state.")
        self._goto_allowed(self.config.login_probe_url)
        self._settle_page()

        for _attempt in range(8):
            self.control.inbox.check_cancelled()
            if self._is_authenticated():
                self.diagnostics.authenticated()
                self.control.status("authenticated", "Yodel session is authenticated.")
                return True

            if self._auth_deadline_passed(auth_deadline_at):
                self.control.status(
                    "auth_deadline",
                    "Yodel was not authenticated before the authentication deadline.",
                )
                return False

            if self._has_otp_challenge():
                self._complete_existing_otp_challenge(auth_deadline_at=auth_deadline_at)
                self._settle_page()
                continue

            if self._has_login_form():
                self._complete_login_form(auth_deadline_at=auth_deadline_at)
                self._settle_page()
                continue

            self.control.status("auth_failed", "Yodel login state was not recognized.")
            return False

        self.control.status(
            "auth_failed",
            "Yodel authentication exceeded the supported number of steps.",
        )
        return False

    def _complete_login_form(self, auth_deadline_at: Optional[datetime] = None) -> None:
        self._assert_page_origin()
        email_input = self._visible_locator(LOGIN_EMAIL_SELECTORS, timeout_ms=500)
        password_input = self._visible_locator(LOGIN_PASSWORD_SELECTORS, timeout_ms=500)
        credentials = self.control.request_credentials()
        try:
            if not credentials.email or not credentials.password:
                raise ActionError(
                    "Yodel requested credentials but none were configured"
                )
            if email_input is not None:
                self._assert_page_origin()
                email_input.fill(credentials.email)
                self._human_pause()
            if password_input is not None:
                self._assert_page_origin()
                password_input.fill(credentials.password)
                self._human_pause()

            self._assert_page_origin()
            submit = self._visible_locator(LOGIN_SUBMIT_SELECTORS, timeout_ms=5_000)
            if submit is None:
                raise ActionError("Yodel login form had no clickable submit control")
            challenge_id = self.control.prepare_otp(
                "login_submit", deadline_at=auth_deadline_at
            )
            self._require_auth_window(auth_deadline_at, challenge_id)
            try:
                submit.click()
            except Exception as exc:
                self.control.otp_failed(challenge_id, "trigger_failed")
                raise ActionError("Yodel login submit could not be clicked") from exc
            self.control.otp_triggered(challenge_id)
        finally:
            credentials.clear()

        state = self._wait_for_auth_or_otp(auth_deadline_at=auth_deadline_at)
        if state == "authenticated":
            self.control.otp_not_required(challenge_id)
            return
        if state == "login":
            # Yodel can split email and password across separate Continue
            # screens. The current click did not generate MFA; the outer auth
            # loop will handle the next credential screen.
            self.control.otp_not_required(challenge_id)
            return
        if state == "deadline":
            self.control.otp_failed(challenge_id, "auth_deadline")
            raise ActionError(
                "Yodel was not authenticated before the authentication deadline"
            )
        if state != "otp":
            self.control.otp_failed(challenge_id, "challenge_not_visible")
            raise ActionError("Yodel did not show an OTP challenge after login")
        self._fill_and_submit_otp(challenge_id, auth_deadline_at=auth_deadline_at)

    def _complete_existing_otp_challenge(
        self, auth_deadline_at: Optional[datetime] = None
    ) -> None:
        self._assert_page_origin()
        resend = self._visible_locator(OTP_REQUEST_SELECTORS, timeout_ms=1_000)
        if resend is None:
            raise ActionError(
                "An existing Yodel OTP challenge cannot be safely refreshed"
            )
        challenge_id = self.control.prepare_otp("resend", deadline_at=auth_deadline_at)
        self._require_auth_window(auth_deadline_at, challenge_id)
        try:
            resend.click()
        except Exception as exc:
            self.control.otp_failed(challenge_id, "trigger_failed")
            raise ActionError("Yodel OTP resend could not be clicked") from exc
        self.control.otp_triggered(challenge_id)
        self._settle_page(timeout_ms=5_000)
        self._fill_and_submit_otp(challenge_id, auth_deadline_at=auth_deadline_at)

    def _fill_and_submit_otp(
        self,
        challenge_id: str,
        auth_deadline_at: Optional[datetime] = None,
    ) -> None:
        self._assert_page_origin()
        value = self.control.wait_for_otp(challenge_id, deadline_at=auth_deadline_at)
        try:
            self._assert_page_origin()
            inputs = self._otp_inputs()
            if not inputs:
                self.control.otp_failed(challenge_id, "input_missing")
                raise ActionError("Yodel OTP challenge had no fillable input")
            self._require_auth_window(auth_deadline_at, challenge_id)
            if len(inputs) >= len(value) and self._looks_like_split_otp(inputs):
                for index, digit in enumerate(value):
                    inputs[index].fill(digit)
                    self._human_pause(0.03, 0.12)
            else:
                inputs[0].fill(value)
            self._human_pause(0.2, 0.8)
            submit = self._visible_locator(OTP_SUBMIT_SELECTORS, timeout_ms=5_000)
            if self._auth_deadline_passed(auth_deadline_at):
                self._clear_otp_inputs(inputs)
                self.control.otp_failed(challenge_id, "auth_deadline")
                raise ActionError(
                    "Authentication deadline passed before the OTP could be submitted"
                )
            if submit is not None:
                submit.click()
                self._human_pause()
            else:
                self.page.keyboard.press("Enter")
            self.control.otp_submitted(challenge_id)
        finally:
            value = ""

    def _wait_for_auth_or_otp(
        self,
        timeout_seconds: float = 15.0,
        auth_deadline_at: Optional[datetime] = None,
    ) -> str:
        deadline = time.monotonic() + timeout_seconds
        while time.monotonic() < deadline:
            self.control.inbox.check_cancelled()
            if self._has_otp_challenge(timeout_ms=200):
                return "otp"
            if self._is_authenticated():
                return "authenticated"
            if self._auth_deadline_passed(auth_deadline_at):
                return "deadline"
            if self._has_login_form():
                return "login"
            self.page.wait_for_timeout(250)
        return "unknown"

    def wait_for_release_if_needed(self) -> None:
        release_at = self.config.release_at
        if release_at is None:
            return
        waiting_for_future_release = datetime.now(release_at.tzinfo) < release_at
        if waiting_for_future_release:
            self.diagnostics.suspend_trace()
        # Trigger both maintenance actions on the first loop iteration without
        # assuming the host has been up for at least 45 seconds. Fresh CI
        # runners and newly booted machines can have a smaller monotonic clock.
        last_keepalive = float("-inf")
        last_heartbeat = float("-inf")
        self.control.status(
            "release_wait", "Authenticated; waiting for the booking release time."
        )
        while datetime.now(release_at.tzinfo) < release_at:
            self.control.inbox.check_cancelled()
            now = time.monotonic()
            if now - last_heartbeat >= 15.0:
                self.control.heartbeat("release_wait")
                last_heartbeat = now
            if now - last_keepalive >= 45.0:
                try:
                    self.keep_session_warm(
                        auth_deadline_at=self.config.auth_deadline_at
                    )
                finally:
                    if waiting_for_future_release:
                        self.diagnostics.suspend_trace()
                last_keepalive = now
            remaining = max(
                0.0, (release_at - datetime.now(release_at.tzinfo)).total_seconds()
            )
            time.sleep(min(1.0, remaining))
        if not self._is_authenticated():
            if self._auth_deadline_passed(self.config.auth_deadline_at):
                raise ActionError(
                    "Yodel session expired after the authentication deadline"
                )
            try:
                authenticated = self.ensure_authenticated(
                    auth_deadline_at=self.config.auth_deadline_at
                )
            finally:
                if waiting_for_future_release:
                    self.diagnostics.suspend_trace()
            if not authenticated:
                raise ActionError("Yodel session was not ready at release time")
        if waiting_for_future_release:
            self.diagnostics.authenticated()
        self.control.emit("release.ready")

    def keep_session_warm(self, auth_deadline_at: Optional[datetime] = None) -> None:
        try:
            self.page.evaluate("() => document.title")
            if random.random() < 0.35:
                self._assert_page_origin()
                self.page.reload(wait_until="domcontentloaded")
                self._assert_page_origin()
                self._settle_page(timeout_ms=5_000)
            if not self._is_authenticated():
                if self._auth_deadline_passed(auth_deadline_at):
                    self.control.status(
                        "auth_expired",
                        "Yodel session expired after the authentication deadline.",
                    )
                    raise ActionError(
                        "Yodel session expired after the authentication deadline"
                    )
                self.control.status(
                    "reauth", "Yodel session expired; re-authenticating."
                )
                if not self.ensure_authenticated(auth_deadline_at=auth_deadline_at):
                    raise ActionError("Yodel session expired before release")
        except ActionError:
            raise
        except Exception as exc:
            raise ActionError("Yodel session keepalive failed") from exc

    def try_booking_once(self, mode: str) -> BookingResult:
        for preference in build_pass_order(self.config.pass_order):
            self.control.inbox.check_cancelled()
            result = self._try_pass(preference, mode=mode)
            if result.success:
                return result
        return BookingResult(False, "No selected pass was available or actionable.")

    def poll_for_booking(self, mode: str) -> BookingResult:
        deadline = time.monotonic() + self.config.poll_deadline_seconds
        attempt = 0
        last_message = "No attempts made."
        while time.monotonic() < deadline:
            self.control.inbox.check_cancelled()
            attempt += 1
            self.control.emit("booking.poll", attempt=attempt)
            result = self.try_booking_once(mode=mode)
            last_message = result.message
            if result.success:
                return result
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            sleep_for = min(
                random.uniform(
                    self.config.poll_min_seconds, self.config.poll_max_seconds
                ),
                remaining,
            )
            self._cancellable_sleep(sleep_for, phase="booking_poll")
        self._capture_failure("poll-deadline")
        return BookingResult(
            False, f"Polling deadline reached. Last status: {last_message}"
        )

    def _try_pass(self, preference: PassPreference, mode: str) -> BookingResult:
        url = self._url_for(preference)
        self.control.status(
            "checking_pass", f"Checking {preference.label} pass availability."
        )
        self._goto_allowed(url)
        self._settle_page(timeout_ms=10_000)

        if not self._select_target_date():
            return BookingResult(
                False,
                f"Target date {self.config.target_date} was not selectable.",
                preference.key,
            )

        container = self._find_pass_container(preference)
        if container is None:
            return BookingResult(
                False, f"{preference.label} pass card was not found.", preference.key
            )
        if not self._pass_is_available(container):
            return BookingResult(
                False, f"{preference.label} pass is not available.", preference.key
            )
        if not self._select_vehicle(container):
            self._capture_failure(f"{preference.key}-vehicle-not-found")
            return BookingResult(
                False,
                f"{preference.label} pass was available, but the vehicle was not selected.",
                preference.key,
            )

        if mode == "dry-run":
            self._capture_failure(f"{preference.key}-dry-run-ready")
            return BookingResult(
                True,
                f"{preference.label} pass and vehicle selection were verified in dry-run.",
                preference.key,
            )

        if not self._click_first(container, ADD_TO_CART_SELECTORS, timeout_ms=5_000):
            if not self._click_first(
                self.page, ADD_TO_CART_SELECTORS, timeout_ms=5_000
            ):
                self._capture_failure(f"{preference.key}-add-to-cart-failed")
                return BookingResult(
                    False,
                    f"{preference.label} Add To Cart was not clickable.",
                    preference.key,
                )

        self._human_pause(0.4, 1.2)
        if not self._click_first(self.page, CHECKOUT_SELECTORS, timeout_ms=15_000):
            self._capture_failure(f"{preference.key}-checkout-failed")
            return BookingResult(
                False, f"{preference.label} checkout was not clickable.", preference.key
            )

        final_confirm = self._visible_locator(
            FINAL_CONFIRM_SELECTORS, timeout_ms=30_000
        )
        if final_confirm is None:
            self._capture_failure(f"{preference.key}-final-confirm-not-found")
            return BookingResult(
                False,
                f"{preference.label} final confirmation was not available.",
                preference.key,
            )

        if mode == "manual":
            self._capture_failure(f"{preference.key}-manual-confirm-ready")
            self.diagnostics.suspend_trace()
            self.control.wait_for_approval(
                pass_key=preference.key,
                label=preference.label,
                browser_is_ready=lambda: self._manual_confirmation_is_ready(
                    final_confirm
                ),
            )
            self.diagnostics.authenticated()
        elif mode != "auto":
            raise ActionError("Unsupported booking mode")

        self._click_final_confirmation(final_confirm, preference)
        self._capture_failure(f"{preference.key}-confirmed")
        return BookingResult(
            True, f"{preference.label} pass checkout was confirmed.", preference.key
        )

    def _click_final_confirmation(
        self, locator: Any, preference: PassPreference
    ) -> None:
        self.control.inbox.check_cancelled()
        self._assert_page_origin()
        confirmation_id = self.control.await_confirmation_ready(
            preference.key, preference.label
        )
        try:
            locator.click(timeout=30_000)
        except Exception as exc:
            raise OutcomeUnknown(
                f"Final confirmation for {preference.label} may have been submitted; do not retry automatically"
            ) from exc
        self.control.emit(
            "confirmation.completed",
            confirmation_id=confirmation_id,
            pass_key=preference.key,
            label=preference.label,
        )

    def _goto_allowed(self, url: str) -> None:
        if not self.config.allows_yodel_url(url):
            raise ActionError("Refusing to navigate outside the approved Yodel origin")
        self.page.goto(url, wait_until="domcontentloaded")
        self._assert_page_origin()

    def _assert_page_origin(self) -> None:
        if not self.config.allows_yodel_url(str(self.page.url)):
            raise ActionError("Yodel left the approved credential origin")

    def _guard_navigation(self, route: Any, request: Any) -> None:
        try:
            top_level = (
                request.is_navigation_request()
                and request.frame == self.page.main_frame
            )
            if top_level and not self.config.allows_yodel_url(request.url):
                route.abort("blockedbyclient")
                return
            route.continue_()
        except Exception:
            # A navigation whose metadata cannot be proven safe must not be
            # allowed to proceed. Do not log the requested URL.
            try:
                route.abort("blockedbyclient")
            except Exception:
                pass

    def _manual_confirmation_is_ready(self, locator: Any) -> bool:
        try:
            if self.page.is_closed():
                return False
            return bool(
                locator.is_visible(timeout=500) and locator.is_enabled(timeout=500)
            )
        except Exception:
            return False

    def _url_for(self, preference: PassPreference) -> str:
        if preference.url_kind == "all_day" and self.config.all_day_pass_url:
            return self.config.all_day_pass_url
        if preference.url_kind == "half_day" and self.config.half_day_pass_url:
            return self.config.half_day_pass_url
        raise ActionError(f"No URL was configured for {preference.label}.")

    def _has_login_form(self) -> bool:
        return (
            self._visible_locator(LOGIN_EMAIL_SELECTORS, timeout_ms=300) is not None
            or self._visible_locator(LOGIN_PASSWORD_SELECTORS, timeout_ms=300)
            is not None
        )

    def _is_authenticated(self) -> bool:
        if self._visible_locator(LOGIN_EMAIL_SELECTORS, timeout_ms=150) is not None:
            return False
        if self._visible_locator(LOGIN_PASSWORD_SELECTORS, timeout_ms=150) is not None:
            return False
        if self._has_otp_challenge(timeout_ms=150):
            return False
        return (
            self._visible_locator(AUTHENTICATED_SELECTORS, timeout_ms=750) is not None
        )

    def _has_otp_challenge(self, timeout_ms: int = 500) -> bool:
        return (
            self._visible_locator(OTP_INPUT_SELECTORS, timeout_ms=timeout_ms)
            is not None
        )

    def _otp_inputs(self) -> list[Any]:
        locators: list[Any] = []
        for selector in OTP_INPUT_SELECTORS:
            locator = self.page.locator(selector)
            try:
                count = min(locator.count(), 8)
            except Exception:
                continue
            for index in range(count):
                item = locator.nth(index)
                try:
                    if item.is_visible(timeout=1_000) and item.is_enabled(
                        timeout=1_000
                    ):
                        locators.append(item)
                except Exception:
                    continue
            if locators:
                return locators
        return locators

    def _looks_like_split_otp(self, inputs: list[Any]) -> bool:
        if len(inputs) < 4:
            return False
        for item in inputs[:4]:
            try:
                if (
                    item.get_attribute("maxlength") == "1"
                    or item.get_attribute("size") == "1"
                ):
                    return True
            except Exception:
                continue
        return False

    def _select_target_date(self) -> bool:
        target = self.config.target_date
        day = str(target.day)
        exact_tokens = {
            target.isoformat(),
            target.strftime("%B %d").replace(" 0", " "),
            target.strftime("%b %d").replace(" 0", " "),
            target.strftime("%A, %B %d").replace(" 0", " "),
        }
        buttons = self.page.locator(".datelist button.date, button.date")
        try:
            count = buttons.count()
        except Exception:
            count = 0
        fallback = None
        for index in range(count):
            button = buttons.nth(index)
            try:
                text = button.inner_text(timeout=500).strip()
                attrs = " ".join(
                    value or ""
                    for value in (
                        button.get_attribute("aria-label"),
                        button.get_attribute("title"),
                        button.get_attribute("data-date"),
                        button.get_attribute("datetime"),
                    )
                )
                combined = f"{text} {attrs}"
                if any(token.lower() in combined.lower() for token in exact_tokens):
                    button.click()
                    self._human_pause(0.1, 0.5)
                    return True
                if text == day and fallback is None:
                    fallback = button
            except Exception:
                continue
        if fallback is not None:
            fallback.click()
            self._human_pause(0.1, 0.5)
            return True
        for selector in DATE_BUTTON_SELECTORS:
            locator = self._visible_locator(
                (selector.format(day=day),), timeout_ms=1_000
            )
            if locator is not None:
                locator.click()
                self._human_pause(0.1, 0.5)
                return True
        return False

    def _find_pass_container(self, preference: PassPreference) -> Optional[Any]:
        for pattern in preference.text_patterns:
            selectors = (
                f".card.ImageCard:has-text('{pattern}')",
                f".card:has-text('{pattern}')",
                f"[class*='card' i]:has-text('{pattern}')",
            )
            locator = self._visible_locator(selectors, timeout_ms=1_000)
            if locator is not None:
                return locator
        regex = re.compile(
            "|".join(re.escape(pattern) for pattern in preference.text_patterns), re.I
        )
        try:
            text_locator = self.page.get_by_text(regex).first
            text_locator.wait_for(state="visible", timeout=1_000)
            ancestor = text_locator.locator(
                "xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' card ') "
                "or contains(@class, 'ImageCard')][1]"
            )
            if ancestor.count() > 0:
                return ancestor.first
        except Exception:
            return None
        return None

    def _pass_is_available(self, container: Any) -> bool:
        try:
            text = container.inner_text(timeout=2_000).lower()
        except Exception:
            text = ""
        if any(
            token in text
            for token in ("sold out", "unavailable", "not available", "full")
        ):
            return False
        if (
            self._visible_locator(
                ADD_TO_CART_SELECTORS, root=container, timeout_ms=1_000
            )
            is not None
        ):
            return True
        return "available" in text

    def _select_vehicle(self, container: Any) -> bool:
        keyword = self.config.vehicle_keyword.lower()
        if not self._click_first(
            container, VEHICLE_SELECTOR_SELECTORS, timeout_ms=3_000
        ):
            self._click_first(self.page, VEHICLE_SELECTOR_SELECTORS, timeout_ms=3_000)
        self._human_pause(0.3, 1.0)
        popup = self._visible_locator(
            (
                ".popup.smart-select-popup.modal-in",
                ".smart-select-popup",
                ".modal-in",
                "[role='dialog']",
            ),
            timeout_ms=5_000,
        )
        root = popup if popup is not None else self.page
        labels = root.locator("label.item-radio, label:has(.item-title), label")
        try:
            count = labels.count()
        except Exception:
            count = 0
        for index in range(count):
            label = labels.nth(index)
            try:
                text = label.inner_text(timeout=500).strip()
                if keyword in text.lower():
                    label.click()
                    self._human_pause(0.2, 0.7)
                    self._close_vehicle_popup_if_open()
                    return True
            except Exception:
                continue
        selects = self.page.locator("select")
        try:
            select_count = selects.count()
        except Exception:
            select_count = 0
        for index in range(select_count):
            select = selects.nth(index)
            try:
                options = select.locator("option")
                for option_index in range(options.count()):
                    option = options.nth(option_index)
                    label = option.inner_text(timeout=500).strip()
                    value = option.get_attribute("value") or label
                    if keyword in label.lower():
                        select.select_option(value=value)
                        return True
            except Exception:
                continue
        return False

    def _close_vehicle_popup_if_open(self) -> None:
        self._click_first(
            self.page,
            (
                ".link.popup-close",
                "a.popup-close",
                "button:has-text('Done')",
                "button:has-text('Close')",
            ),
            timeout_ms=1_000,
        )

    def _click_first(
        self, root: Any, selectors: Iterable[str], timeout_ms: int
    ) -> bool:
        locator = self._visible_locator(selectors, root=root, timeout_ms=timeout_ms)
        if locator is None:
            return False
        try:
            locator.click()
            self._human_pause()
            return True
        except Exception:
            return False

    def _visible_locator(
        self,
        selectors: Iterable[str],
        root: Optional[Any] = None,
        timeout_ms: int = 1_000,
    ) -> Optional[Any]:
        search_root = root if root is not None else self.page
        for selector in selectors:
            try:
                locator = search_root.locator(selector).first
                locator.wait_for(state="visible", timeout=timeout_ms)
                return locator
            except Exception:
                continue
        return None

    def _settle_page(self, timeout_ms: int = 15_000) -> None:
        try:
            self.page.wait_for_load_state("domcontentloaded", timeout=timeout_ms)
            self.page.locator("body").wait_for(
                state="visible", timeout=min(timeout_ms, 5_000)
            )
            self.page.wait_for_timeout(500)
        except Exception:
            logger.debug("Page did not fully settle before its bounded deadline")

    def _capture_failure(self, name: str) -> None:
        self.diagnostics.screenshot(self.page, name)

    def _human_pause(self, minimum: float = 0.15, maximum: float = 0.65) -> None:
        time.sleep(random.uniform(minimum, maximum))

    def _cancellable_sleep(self, seconds: float, phase: str) -> None:
        deadline = time.monotonic() + seconds
        last_heartbeat = time.monotonic()
        while time.monotonic() < deadline:
            self.control.inbox.check_cancelled()
            if time.monotonic() - last_heartbeat >= 15.0:
                self.control.heartbeat(phase)
                last_heartbeat = time.monotonic()
            time.sleep(min(0.25, max(0.0, deadline - time.monotonic())))

    def _require_auth_window(
        self,
        auth_deadline_at: Optional[datetime],
        challenge_id: str,
    ) -> None:
        if self._auth_deadline_passed(auth_deadline_at):
            self.control.otp_failed(challenge_id, "auth_deadline")
            raise ActionError(
                "Authentication deadline passed before a new OTP could be triggered"
            )

    @staticmethod
    def _clear_otp_inputs(inputs: list[Any]) -> None:
        for item in inputs:
            try:
                item.fill("")
            except Exception:
                continue

    @staticmethod
    def _auth_deadline_passed(auth_deadline_at: Optional[datetime]) -> bool:
        if auth_deadline_at is None:
            return False
        return datetime.now(auth_deadline_at.tzinfo) >= auth_deadline_at
