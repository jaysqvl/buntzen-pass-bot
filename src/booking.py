import logging
from selenium.common.exceptions import TimeoutException, NoSuchElementException
from selenium.webdriver.common.by import By
from selenium.webdriver.support.ui import WebDriverWait
from selenium.webdriver.support import expected_conditions as EC
import time
import threading
import random
from typing import Callable, Dict, Any
from datetime import datetime
from .selenium_utils import robust_find_and_act
from .chrome_utils import open_new_tab, switch_to_tab

logger = logging.getLogger("buntzen_pass_bot.booking")

def find_pass_card(driver, pass_type_label):
    """
    Find the card element for the given pass type (e.g., 'Morning', 'Afternoon').
    Returns the card WebElement or None if not found.
    """
    cards = driver.find_elements(By.CSS_SELECTOR, ".card.ImageCard")
    for card in cards:
        try:
            label = card.find_element(By.CSS_SELECTOR, ".Cardimage .text").text
            if pass_type_label.lower() in label.lower():
                return card
        except Exception:
            continue
    return None

def is_pass_sold_out(card):
    """
    Check if the Add to Cart button in the card says 'Sold out'.
    Returns True if sold out, False otherwise.
    """
    try:
        add_to_cart_btn = card.find_element(By.CSS_SELECTOR, ".addCartBtnClassification a")
        return "sold out" in add_to_cart_btn.text.lower()
    except Exception:
        return True  # If button not found, treat as sold out

def select_date(driver, target_date):
    """
    Clicks the date button for the target date (YYYY-MM-DD) on the current page.
    Returns True if successful, False otherwise.
    """
    try:
        reload_attempts = 0
        while True:
            try:
                robust_find_and_act(driver, By.CSS_SELECTOR, ".datelist button.date", wait_condition='present', timeout=5, retries=2)
                break
            except Exception as e:
                reload_attempts += 1
                logger.warning(f"Date buttons not found after 5s, reloading page (attempt {reload_attempts})...")
                driver.refresh()
                time.sleep(1)
        date_buttons = driver.find_elements(By.CSS_SELECTOR, ".datelist button.date")
        found = False
        target_day = str(int(target_date.split('-')[2]))  # Remove leading zero
        for btn in date_buttons:
            if btn.text.strip() == target_day:
                if 'active' not in btn.get_attribute('class'):
                    logger.debug(f"Clicking date button for {target_day}")
                    robust_find_and_act(driver, By.CSS_SELECTOR, f".datelist button.date:nth-child({date_buttons.index(btn)+1})", action=lambda el: el.click(), wait_condition='clickable', timeout=5, retries=5)
                    WebDriverWait(driver, 5).until(
                        lambda d: 'active' in btn.get_attribute('class')
                    )
                else:
                    logger.debug(f"Date button for {target_day} is already active.")
                found = True
                break
        if not found:
            logger.warning(f"Date button for {target_day} not found. Retrying...")
            time.sleep(2)
            return False
        logger.info(f"Correct date button for {target_day} is now active.")
        return True
    except Exception as e:
        logger.error(f"Error selecting date: {e}")
        return False

def book_all_day_pass(driver, config, select_vehicle_and_checkout):
    logger.info(f"Opening All Day Pass URL: {config['ALL_DAY_PASS_URL']}")
    driver.get(config['ALL_DAY_PASS_URL'])
    logger.info("Waiting for date buttons to load...")
    if not select_date(driver, config['TARGET_DATE']):
        return
    logger.info("Checking All Day Pass...")
    try:
        all_day_pass = robust_find_and_act(driver, By.XPATH, "//*[contains(text(), 'All-day Pass (8 a.m. to 8:00 p.m.)')]", wait_condition='visible', timeout=10, retries=10)
        logger.info("Found All Day Pass element.")
        availability_status = robust_find_and_act(all_day_pass, By.XPATH, "../..//*[contains(text(), 'Available')]", wait_condition='visible', timeout=5, retries=5)
        logger.info("Found availability status for All Day Pass.")
        if availability_status:
            logger.info("All Day Pass is available! Proceeding to add to cart...")
            if select_vehicle_and_checkout(driver, all_day_pass, config['VEHICLE_KEYWORD']):
                logger.info("Successfully checked out All Day Pass.")
                return True
        else:
            logger.info("All Day Pass is not available.")
    except (TimeoutException, NoSuchElementException) as e:
        logger.info(f"All Day Pass is sold out or not loaded yet. Exception: {e}.")
    return False

def is_target_date_present(driver, target_date: str) -> bool:
    """
    Returns True if the target date button is present on the page, False otherwise.
    """
    try:
        date_buttons = driver.find_elements(By.CSS_SELECTOR, ".datelist button.date")
        target_day = str(int(target_date.split('-')[2]))
        for btn in date_buttons:
            if btn.text.strip() == target_day:
                return True
        return False
    except Exception as e:
        logger.warning(f"Error checking for target date button: {e}")
        return False

def book_half_day_passes_parallel(
    driver: Any,
    config: Dict[str, Any],
    select_vehicle_and_checkout: Callable[[Any, Any, str], bool]
) -> None:
    """
    Try to book half-day passes using parallel threads for each pass type enabled in config.
    Afternoon is prioritized first if both are enabled.
    """
    def parallel_halfday_booking_worker(
        driver: Any,
        tab_index: int,
        stop_event: threading.Event,
        reload_interval: float,
        config: Dict[str, Any],
        select_vehicle_and_checkout: Callable[[Any, Any, str], bool],
        winner_info: Dict[str, Any],
        pass_type: str
    ) -> None:
        switch_to_tab(driver, tab_index)
        logger.info(f"[THREAD-{tab_index}] Started with reload interval {reload_interval}s for {pass_type} pass")
        try:
            driver.get(config['HALF_DAY_PASS_URL'])
            # Wait for date elements to load
            WebDriverWait(driver, 10).until(
                EC.presence_of_all_elements_located((By.CSS_SELECTOR, ".datelist button.date"))
            )
            # If the target date is not present, exit immediately (no reloads)
            if not is_target_date_present(driver, config['TARGET_DATE']):
                logger.error(f"[THREAD-{tab_index}] Target date {config['TARGET_DATE']} not found on page. Exiting to avoid unnecessary requests. Enable scheduling if you want to wait for the date.")
                stop_event.set()
                return
            select_date(driver, config['TARGET_DATE'])
            while not stop_event.is_set():
                try:
                    card = find_pass_card(driver, pass_type)
                    if card and not is_pass_sold_out(card):
                        logger.info(f"[THREAD-{tab_index}] {pass_type} pass available! Signaling other thread to stop and proceeding with booking.")
                        winner_info['winner'] = tab_index
                        stop_event.set()
                        if select_vehicle_and_checkout(driver, card, config['VEHICLE_KEYWORD']):
                            logger.info(f"[THREAD-{tab_index}] Successfully checked out {pass_type} pass.")
                        else:
                            logger.info(f"[THREAD-{tab_index}] Failed to check out {pass_type} pass after adding to cart.")
                        return
                    else:
                        logger.info(f"[THREAD-{tab_index}] {pass_type} pass not available yet, reloading soon...")
                except Exception as e:
                    logger.info(f"[THREAD-{tab_index}] Exception: {e}")
                jitter = random.uniform(-0.5, 0.5)
                actual_interval = max(0.5, reload_interval + jitter)
                time.sleep(actual_interval)
                driver.refresh()
            if winner_info.get('winner') == tab_index:
                logger.info(f"[THREAD-{tab_index}] This thread won and completed the booking.")
            else:
                logger.info(f"[THREAD-{tab_index}] Stopping because another thread (THREAD-{winner_info.get('winner')}) succeeded.")
        except Exception as e:
            logger.info(f"[THREAD-{tab_index}] Exception: {e}")

    open_new_tab(driver, config['HALF_DAY_PASS_URL'])

    def run_phase(pass_type: str) -> None:
        stop_event = threading.Event()
        winner_info = {}
        thread_fast = threading.Thread(target=parallel_halfday_booking_worker, args=(driver, 0, stop_event, 2, config, select_vehicle_and_checkout, winner_info, pass_type))
        thread_slow = threading.Thread(target=parallel_halfday_booking_worker, args=(driver, 1, stop_event, 5, config, select_vehicle_and_checkout, winner_info, pass_type))
        logger.info(f"[MAIN] Starting parallel booking threads for {pass_type} pass.")
        thread_fast.start()
        thread_slow.start()
        thread_fast.join()
        thread_slow.join()
        logger.info(f"[MAIN] {pass_type} pass booking attempt finished. Winner: THREAD-{winner_info.get('winner')}.")

    if config.get('CHECK_AFTERNOON'):
        run_phase("Afternoon")
    if config.get('CHECK_MORNING'):
        run_phase("Morning")

def main_booking_controller(
    driver: Any,
    config: Dict[str, Any],
    select_vehicle_and_checkout: Callable[[Any, Any, str], bool]
) -> None:
    """
    Modular controller: tries all-day first if enabled, falls back to half-days if enabled and all-day fails.
    Also enforces date distance and SCHEDULE logic.
    """
    # Date distance check
    today = datetime.now().date()
    try:
        target_date = datetime.strptime(config['TARGET_DATE'], "%Y-%m-%d").date()
    except Exception as e:
        logger.error(f"Invalid TARGET_DATE format: {config['TARGET_DATE']}. Should be YYYY-MM-DD. Exiting.")
        return
    days_diff = abs((target_date - today).days)
    if days_diff > 3:
        logger.error(f"Target date {config['TARGET_DATE']} is more than 3 days from today. Exiting to avoid unnecessary requests.")
        return
    # SCHEDULE=false: try to book immediately, but if date not present, exit
    if not config.get('SCHEDULE', False):
        logger.info("SCHEDULE is disabled. Will attempt to book immediately. (For development/testing only!)")
        # Try all-day first if enabled
        if config.get('CHECK_ALL_DAY'):
            logger.info("Trying to book all-day pass...")
            # Check if date is present before proceeding
            driver.get(config['ALL_DAY_PASS_URL'])
            WebDriverWait(driver, 10).until(
                EC.presence_of_all_elements_located((By.CSS_SELECTOR, ".datelist button.date"))
            )
            if not is_target_date_present(driver, config['TARGET_DATE']):
                logger.error(f"Target date {config['TARGET_DATE']} not found on page. Exiting to avoid unnecessary requests. Enable scheduling to wait for the date.")
                return
            success = book_all_day_pass(driver, config, select_vehicle_and_checkout)
            if success:
                logger.info("Successfully booked all-day pass. Exiting.")
                return
            else:
                logger.info("All-day pass not available or sold out.")
                if not (config.get('CHECK_MORNING') or config.get('CHECK_AFTERNOON')):
                    logger.info("No half-day passes enabled. Exiting.")
                    return
                logger.info("Falling back to half-day passes...")
        # Try half-day passes
        book_half_day_passes_parallel(driver, config, select_vehicle_and_checkout)
        return
    # SCHEDULE=true: wait for release time, then proceed as normal
    else:
        logger.info("SCHEDULE is enabled. Will wait for release time and then attempt booking.")
        # Try all-day first if enabled
        if config.get('CHECK_ALL_DAY'):
            logger.info("Trying to book all-day pass...")
            driver.get(config['ALL_DAY_PASS_URL'])
            WebDriverWait(driver, 10).until(
                EC.presence_of_all_elements_located((By.CSS_SELECTOR, ".datelist button.date"))
            )
            if not is_target_date_present(driver, config['TARGET_DATE']):
                logger.error(f"Target date {config['TARGET_DATE']} not found on page after release time. Exiting to avoid unnecessary requests. The booking site may be delayed or there may be a configuration issue.")
                return
            success = book_all_day_pass(driver, config, select_vehicle_and_checkout)
            if success:
                logger.info("Successfully booked all-day pass. Exiting.")
                return
            else:
                logger.info("All-day pass not available or sold out.")
                if not (config.get('CHECK_MORNING') or config.get('CHECK_AFTERNOON')):
                    logger.info("No half-day passes enabled. Exiting.")
                    return
                logger.info("Falling back to half-day passes...")
        # Try half-day passes
        book_half_day_passes_parallel(driver, config, select_vehicle_and_checkout)
        return

def run_booking_flow(driver, config, select_vehicle_and_checkout):
    """
    Main booking loop. Checks for available passes and attempts to book using the provided vehicle selection function.
    Args:
        driver: Selenium WebDriver instance
        config: dict of configuration values
        select_vehicle_and_checkout: function(driver, pass_element, vehicle_keyword) -> bool
    """
    try:
        while True:
            if config['CHECK_ALL_DAY']:
                if book_all_day_pass(driver, config, select_vehicle_and_checkout):
                    break
            elif config['CHECK_MORNING'] or config['CHECK_AFTERNOON']:
                book_half_day_passes_parallel(driver, config, select_vehicle_and_checkout)
            else:
                logger.warning("No pass type selected in config.")
                break
    except Exception as e:
        logger.error(f"An error occurred in booking flow: {e}")
    finally:
        logger.info("Booking flow finished. Closing driver in 5 seconds...")
        time.sleep(5)
        driver.quit() 