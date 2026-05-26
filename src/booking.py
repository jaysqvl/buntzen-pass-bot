from __future__ import annotations

import logging
import random
import re
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Iterable

from playwright.sync_api import Locator, Page, TimeoutError as PlaywrightTimeoutError

from .pass_types import PassPreference, build_pass_order


logger = logging.getLogger("buntzen_pass_bot.booking")


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


class BookingError(RuntimeError):
    pass


@dataclass(frozen=True)
class BookingResult:
    success: bool
    message: str
    pass_key: str | None = None


class BookingBot:
    def __init__(self, page: Page, context, config, twilio, diagnostics, interactive_manual: bool = True) -> None:
        self.page = page
        self.context = context
        self.config = config
        self.twilio = twilio
        self.diagnostics = diagnostics
        self.interactive_manual = interactive_manual

    def alert(self, message: str, urgent: bool = False) -> None:
        logger.info("ALERT: %s", message)
        self.twilio.send_alert(message, urgent=urgent)

    def capture_failure(self, name: str) -> None:
        screenshot = self.diagnostics.screenshot(self.page, name)
        html = self.diagnostics.html(self.page, name)
        logger.info("Saved diagnostics: screenshot=%s html=%s", screenshot, html)

    def ensure_authenticated(self, deadline: datetime | None = None) -> bool:
        logger.info("Checking Yodel authentication state.")
        self.page.goto(self.config.login_probe_url, wait_until="domcontentloaded")
        self._settle_page()

        otp_requested_at: datetime | None = None
        while True:
            if self._is_authenticated():
                logger.info("Yodel session appears authenticated.")
                return True

            if deadline and datetime.now(self.config.timezone) >= deadline:
                self.capture_failure("auth-deadline")
                return False

            if self._has_otp_challenge():
                logger.info("OTP challenge detected.")
                otp_requested_at = otp_requested_at or datetime.now(timezone.utc)
                self._complete_otp_challenge(otp_requested_at)
                self._settle_page()
                continue

            login_clicked_at = self._complete_login_form()
            if login_clicked_at:
                otp_requested_at = login_clicked_at
                self._settle_page()
                continue

            request_clicked_at = self._request_otp_if_possible()
            if request_clicked_at:
                otp_requested_at = request_clicked_at
                self._settle_page()
                continue

            logger.warning("Could not identify authenticated state, login form, or OTP challenge.")
            self.capture_failure("auth-unknown-state")
            return False

    def keep_session_warm(self) -> None:
        try:
            logger.debug("Running session keepalive.")
            self.page.evaluate("() => document.title")
            if random.random() < 0.35:
                self.page.reload(wait_until="domcontentloaded")
                self._settle_page(timeout_ms=5000)
            if not self._is_authenticated():
                logger.warning("Session warm check no longer sees authenticated state; attempting re-auth.")
                self.ensure_authenticated(deadline=self.config.auth_deadline_at)
        except Exception as exc:
            logger.warning("Keepalive failed: %s", exc)

    def try_booking_once(self, mode: str) -> BookingResult:
        for preference in build_pass_order(self.config):
            result = self._try_pass(preference, mode=mode)
            if result.success:
                return result
        return BookingResult(False, "No selected pass was available or actionable.")

    def poll_for_booking(self, mode: str) -> BookingResult:
        deadline = time.monotonic() + self.config.poll_deadline_seconds
        attempt = 0
        last_message = "No attempts made."
        while time.monotonic() < deadline:
            attempt += 1
            logger.info("Booking poll attempt %s", attempt)
            result = self.try_booking_once(mode=mode)
            last_message = result.message
            if result.success:
                return result
            sleep_for = random.uniform(self.config.poll_min_seconds, self.config.poll_max_seconds)
            logger.info("No pass booked yet (%s). Sleeping %.2fs", result.message, sleep_for)
            time.sleep(sleep_for)
        self.capture_failure("poll-deadline")
        return BookingResult(False, f"Polling deadline reached. Last status: {last_message}")

    def _try_pass(self, preference: PassPreference, mode: str) -> BookingResult:
        url = self._url_for(preference)
        logger.info("Checking %s pass at %s", preference.label, url)
        self.page.goto(url, wait_until="domcontentloaded")
        self._settle_page(timeout_ms=10000)

        if not self._select_target_date():
            return BookingResult(False, f"Target date {self.config.target_date} was not selectable.", preference.key)

        container = self._find_pass_container(preference)
        if container is None:
            return BookingResult(False, f"{preference.label} pass card was not found.", preference.key)

        if not self._pass_is_available(container):
            return BookingResult(False, f"{preference.label} pass is not available.", preference.key)

        logger.info("%s pass appears available.", preference.label)
        if not self._select_vehicle(container):
            self.capture_failure(f"{preference.key}-vehicle-not-found")
            return BookingResult(False, f"{preference.label} pass available, but vehicle was not selected.", preference.key)

        if mode == "dry-run":
            self.capture_failure(f"{preference.key}-dry-run-ready")
            return BookingResult(True, f"{preference.label} pass and vehicle selection verified in dry-run.", preference.key)

        if not self._click_first(container, ADD_TO_CART_SELECTORS, timeout_ms=5000):
            if not self._click_first(self.page, ADD_TO_CART_SELECTORS, timeout_ms=5000):
                self.capture_failure(f"{preference.key}-add-to-cart-failed")
                return BookingResult(False, f"{preference.label} pass available, but Add To Cart was not clickable.", preference.key)

        self._human_pause(0.4, 1.2)
        if not self._click_first(self.page, CHECKOUT_SELECTORS, timeout_ms=15000):
            self.capture_failure(f"{preference.key}-checkout-failed")
            return BookingResult(False, f"{preference.label} added, but checkout was not clickable.", preference.key)

        if mode == "manual":
            self.capture_failure(f"{preference.key}-manual-confirm-ready")
            self.alert(f"{preference.label} pass is ready for final confirmation.", urgent=True)
            if self.interactive_manual:
                input("Final confirmation is ready in the browser. Press Enter after reviewing it...")
            return BookingResult(True, f"{preference.label} pass reached manual final confirmation.", preference.key)

        if mode != "auto":
            raise BookingError(f"Unsupported booking mode: {mode}")

        if not self._click_first(self.page, FINAL_CONFIRM_SELECTORS, timeout_ms=30000):
            self.capture_failure(f"{preference.key}-final-confirm-failed")
            return BookingResult(False, f"{preference.label} checkout reached, but final confirmation was not clickable.", preference.key)

        self.capture_failure(f"{preference.key}-confirmed")
        return BookingResult(True, f"{preference.label} pass checkout confirmed.", preference.key)

    def _url_for(self, preference: PassPreference) -> str:
        if preference.url_kind == "all_day" and self.config.all_day_pass_url:
            return self.config.all_day_pass_url
        if preference.url_kind == "half_day" and self.config.half_day_pass_url:
            return self.config.half_day_pass_url
        raise BookingError(f"No URL configured for {preference.label}.")

    def _is_authenticated(self) -> bool:
        if self._visible_locator(LOGIN_EMAIL_SELECTORS, timeout_ms=250) is not None:
            return False
        if self._visible_locator(LOGIN_PASSWORD_SELECTORS, timeout_ms=250) is not None:
            return False
        if self._has_otp_challenge(timeout_ms=250):
            return False
        return self._visible_locator(AUTHENTICATED_SELECTORS, timeout_ms=1000) is not None

    def _complete_login_form(self) -> datetime | None:
        email = self._visible_locator(LOGIN_EMAIL_SELECTORS, timeout_ms=500)
        password = self._visible_locator(LOGIN_PASSWORD_SELECTORS, timeout_ms=500)
        if email is None and password is None:
            return None
        if not self.config.yodel_email or not self.config.yodel_password:
            raise BookingError("Yodel login form is visible, but YODEL_EMAIL/YODEL_PASSWORD are not configured.")
        if email is not None:
            email.fill(self.config.yodel_email)
            self._human_pause()
        if password is not None:
            password.fill(self.config.yodel_password)
            self._human_pause()
        clicked_at = datetime.now(timezone.utc)
        if not self._click_first(self.page, LOGIN_SUBMIT_SELECTORS, timeout_ms=5000):
            raise BookingError("Yodel login form was visible, but no login/continue button was clickable.")
        logger.info("Submitted Yodel login form; waiting for OTP or authenticated state.")
        return clicked_at

    def _request_otp_if_possible(self) -> datetime | None:
        clicked_at = datetime.now(timezone.utc)
        if self._click_first(self.page, OTP_REQUEST_SELECTORS, timeout_ms=500):
            logger.info("Requested OTP code.")
            return clicked_at
        return None

    def _has_otp_challenge(self, timeout_ms: int = 500) -> bool:
        return self._visible_locator(OTP_INPUT_SELECTORS, timeout_ms=timeout_ms) is not None

    def _complete_otp_challenge(self, requested_after: datetime) -> None:
        otp = self.twilio.wait_for_otp(requested_after=requested_after)
        logger.info("Received fresh OTP SMS from Twilio message %s.", otp.sid or "<unknown>")

        inputs = self._otp_inputs()
        if not inputs:
            raise BookingError("OTP challenge was detected, but no OTP input was fillable.")
        if len(inputs) >= len(otp.code) and self._looks_like_split_otp(inputs):
            for index, digit in enumerate(otp.code):
                inputs[index].fill(digit)
                self._human_pause(0.03, 0.12)
        else:
            inputs[0].fill(otp.code)
        self._human_pause(0.2, 0.8)
        if not self._click_first(self.page, OTP_SUBMIT_SELECTORS, timeout_ms=5000):
            self.page.keyboard.press("Enter")

    def _otp_inputs(self) -> list[Locator]:
        locators: list[Locator] = []
        for selector in OTP_INPUT_SELECTORS:
            locator = self.page.locator(selector)
            try:
                count = min(locator.count(), 8)
            except Exception:
                continue
            for index in range(count):
                item = locator.nth(index)
                try:
                    if item.is_visible(timeout=1000) and item.is_enabled(timeout=1000):
                        locators.append(item)
                except Exception:
                    continue
            if locators:
                return locators
        return locators

    def _looks_like_split_otp(self, inputs: list[Locator]) -> bool:
        if len(inputs) < 4:
            return False
        for item in inputs[:4]:
            try:
                maxlength = item.get_attribute("maxlength")
                size = item.get_attribute("size")
                if maxlength == "1" or size == "1":
                    return True
            except Exception:
                continue
        return False

    def _select_target_date(self) -> bool:
        target = self.config.target_date
        day = str(target.day)
        exact_tokens = {
            target.isoformat(),
            target.strftime("%B %-d"),
            target.strftime("%b %-d"),
            target.strftime("%A, %B %-d"),
        }

        buttons = self.page.locator(".datelist button.date, button.date")
        try:
            count = buttons.count()
        except Exception:
            count = 0

        fallback: Locator | None = None
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
            logger.warning("Selecting date by day-of-month fallback: %s", day)
            fallback.click()
            self._human_pause(0.1, 0.5)
            return True

        for selector in DATE_BUTTON_SELECTORS:
            locator = self._visible_locator((selector.format(day=day),), timeout_ms=1000)
            if locator is not None:
                locator.click()
                self._human_pause(0.1, 0.5)
                return True
        return False

    def _find_pass_container(self, preference: PassPreference) -> Locator | None:
        quoted_patterns = ", ".join(preference.text_patterns)
        logger.debug("Looking for pass card containing: %s", quoted_patterns)
        for pattern in preference.text_patterns:
            selectors = (
                f".card.ImageCard:has-text('{pattern}')",
                f".card:has-text('{pattern}')",
                f"[class*='card' i]:has-text('{pattern}')",
            )
            locator = self._visible_locator(selectors, timeout_ms=1000)
            if locator is not None:
                return locator

        regex = re.compile("|".join(re.escape(pattern) for pattern in preference.text_patterns), re.I)
        text_locator = self.page.get_by_text(regex).first
        try:
            text_locator.wait_for(state="visible", timeout=1000)
            ancestor = text_locator.locator(
                "xpath=ancestor::*[contains(concat(' ', normalize-space(@class), ' '), ' card ') or contains(@class, 'ImageCard')][1]"
            )
            if ancestor.count() > 0:
                return ancestor.first
        except Exception:
            return None
        return None

    def _pass_is_available(self, container: Locator) -> bool:
        try:
            text = container.inner_text(timeout=2000).lower()
        except Exception:
            text = ""
        unavailable_tokens = ("sold out", "unavailable", "not available", "full")
        if any(token in text for token in unavailable_tokens):
            return False
        if self._visible_locator(ADD_TO_CART_SELECTORS, root=container, timeout_ms=1000) is not None:
            return True
        return "available" in text

    def _select_vehicle(self, container: Locator) -> bool:
        keyword = self.config.vehicle_keyword.lower()
        if not self._click_first(container, VEHICLE_SELECTOR_SELECTORS, timeout_ms=3000):
            self._click_first(self.page, VEHICLE_SELECTOR_SELECTORS, timeout_ms=3000)
        self._human_pause(0.3, 1.0)

        popup = self._visible_locator(
            (
                ".popup.smart-select-popup.modal-in",
                ".smart-select-popup",
                ".modal-in",
                "[role='dialog']",
            ),
            timeout_ms=5000,
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
                    logger.info("Selecting vehicle matching keyword: %s", text)
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
            timeout_ms=1000,
        )

    def _click_first(self, root, selectors: Iterable[str], timeout_ms: int) -> bool:
        locator = self._visible_locator(selectors, root=root, timeout_ms=timeout_ms)
        if locator is None:
            return False
        try:
            locator.click()
            self._human_pause()
            return True
        except Exception as exc:
            logger.debug("Click failed for visible locator: %s", exc)
            return False

    def _visible_locator(
        self,
        selectors: Iterable[str],
        root=None,
        timeout_ms: int = 1000,
    ) -> Locator | None:
        search_root = root if root is not None else self.page
        for selector in selectors:
            try:
                locator = search_root.locator(selector).first
                locator.wait_for(state="visible", timeout=timeout_ms)
                return locator
            except PlaywrightTimeoutError:
                continue
            except Exception as exc:
                logger.debug("Selector failed %s: %s", selector, exc)
                continue
        return None

    def _settle_page(self, timeout_ms: int = 15000) -> None:
        try:
            self.page.wait_for_load_state("domcontentloaded", timeout=timeout_ms)
            self.page.locator("body").wait_for(state="visible", timeout=min(timeout_ms, 5000))
            self.page.wait_for_timeout(500)
        except PlaywrightTimeoutError:
            logger.debug("Page did not reach DOM/body readiness within %sms; continuing.", timeout_ms)

    def _human_pause(self, minimum: float = 0.15, maximum: float = 0.65) -> None:
        time.sleep(random.uniform(minimum, maximum))
