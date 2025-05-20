import undetected_chromedriver as uc
from fake_useragent import UserAgent
from dotenv import load_dotenv
import time
import os
from datetime import datetime, timedelta, time as dt_time
import pytz
import threading
import random

from src.env_utils import load_config
from src.vehicle_selector import select_vehicle_and_checkout
from src.booking import run_booking_flow
from src.scheduler import get_ntp_time, get_seconds_until, get_days_until
from src.chrome_utils import open_new_tab, switch_to_tab

def log(msg):
    print(f"[{datetime.now().strftime('%Y-%m-%d %H:%M:%S')}] {msg}")

# Load environment variables from .env file
load_dotenv(override=True)

# Load config
config = load_config()

# --- Scheduling for Pass Release ---
# Only use scheduling if SCHEDULE is true and TARGET_DATE is set
if config.get('SCHEDULE') and config.get('TARGET_DATE'):
    # Calculate the relevant times in PST on the day before TARGET_DATE
    target_date = datetime.strptime(config['TARGET_DATE'], "%Y-%m-%d")
    release_date = target_date - timedelta(days=1)
    pst = pytz.timezone('US/Pacific')

    # Parse SLOW_POLL_UNTIL and START_TIME as times
    slow_poll_until_time = datetime.strptime(config['SLOW_POLL_UNTIL'], "%H:%M").time()
    start_time = datetime.strptime(config['START_TIME'], "%H:%M").time()

    # Compose datetimes in PST
    slow_poll_until_pst = pst.localize(datetime.combine(release_date, slow_poll_until_time))
    start_time_pst = pst.localize(datetime.combine(release_date, start_time))

    # Convert to UTC for comparison with NTP time
    slow_poll_until_utc = slow_poll_until_pst.astimezone(pytz.utc)
    start_time_utc = start_time_pst.astimezone(pytz.utc)

    print(f"Target pass date: {config['TARGET_DATE']}")
    print(f"Will slow poll until: {slow_poll_until_pst.strftime('%Y-%m-%d %H:%M:%S %Z')} (PST)")
    print(f"Will start booking flow at: {start_time_pst.strftime('%Y-%m-%d %H:%M:%S %Z')} (PST)")

    # Phase 1: Slow polling (every 1s) until SLOW_POLL_UNTIL
    while True:
        now_utc = get_ntp_time().replace(tzinfo=pytz.utc)
        if now_utc >= slow_poll_until_utc:
            break
        seconds_until_slow_poll = (slow_poll_until_utc - now_utc).total_seconds()
        print(f"[Slow Poll] Waiting for fast poll window... {int(seconds_until_slow_poll)} seconds remaining.")
        time.sleep(1)

    # Phase 2: Fast polling (every 0.05s) until START_TIME
    while True:
        now_utc = get_ntp_time().replace(tzinfo=pytz.utc)
        if now_utc >= start_time_utc:
            print(f"It's time! {now_utc.strftime('%Y-%m-%d %H:%M:%S %Z')} (UTC)")
            break
        seconds_until_start = (start_time_utc - now_utc).total_seconds()
        if int(seconds_until_start * 100) % 10 == 0:  # Print every 0.5s
            print(f"[Fast Poll] Waiting for booking start... {seconds_until_start:.2f} seconds remaining.")
        time.sleep(0.05)

# Generate a random user-agent
ua = UserAgent()
user_agent = ua.random

# Set up Chrome options with the random user-agent and your existing profile
driver_options = uc.ChromeOptions()
driver_options.add_argument('--no-sandbox')
driver_options.add_argument('--disable-dev-shm-usage')
driver_options.add_argument("--disable-gpu")
driver_options.add_argument('--enable-javascript')
driver_options.add_argument(f'--user-agent={user_agent}')

# Ensure the user data directory exists
os.makedirs(config["USER_DATA_DIR"], exist_ok=True)
driver_options.add_argument(f'--user-data-dir={config["USER_DATA_DIR"]}')

print("Loaded config:", config)
print(f"User agent: {user_agent}")
print(f"Chrome will use user data dir: {config['USER_DATA_DIR']}")
print("Initializing Chrome driver...")
driver = uc.Chrome(options=driver_options, use_subprocess=True)
print("Chrome driver initialized. Opening Browser...")

# Prompt user to log in manually before continuing
print("Please log in to the Buntzen website in the opened browser window.")
input("Once you have successfully logged in, press Enter here to continue...")

# --- Parallel Booking Logic ---

def parallel_booking_worker(driver, tab_index, stop_event, reload_interval, config, select_vehicle_and_checkout, winner_info):
    switch_to_tab(driver, tab_index)
    log(f"[THREAD-{tab_index}] Started with reload interval {reload_interval}s")
    try:
        from src.selenium_utils import robust_find_and_act
        from selenium.webdriver.common.by import By
        # Go to the all-day pass page
        driver.get(config['ALL_DAY_PASS_URL'])
        while not stop_event.is_set():
            try:
                # Try to find date buttons quickly
                robust_find_and_act(driver, By.CSS_SELECTOR, ".datelist button.date", wait_condition='present', timeout=3, retries=2)
                log(f"[THREAD-{tab_index}] Date buttons found! Signaling other thread to stop and proceeding with booking.")
                winner_info['winner'] = tab_index
                stop_event.set()
                # Proceed with the normal booking flow in this tab
                run_booking_flow(driver, config, select_vehicle_and_checkout)
                log(f"[THREAD-{tab_index}] Finished booking flow.")
                return
            except Exception as e:
                jitter = random.uniform(-0.5, 0.5)
                actual_interval = max(0.5, reload_interval + jitter)
                log(f"[THREAD-{tab_index}] Date buttons not found, reloading in {actual_interval:.2f}s...")
                time.sleep(actual_interval)
                driver.refresh()
        if winner_info.get('winner') == tab_index:
            log(f"[THREAD-{tab_index}] This thread won and completed the booking.")
        else:
            log(f"[THREAD-{tab_index}] Stopping because another thread (THREAD-{winner_info.get('winner')}) succeeded.")
    except Exception as e:
        log(f"[THREAD-{tab_index}] Exception: {e}")

# Open a second tab
open_new_tab(driver, config['ALL_DAY_PASS_URL'])

# Set up threading event and winner info
top_event = threading.Event()
winner_info = {}

# Start two threads: one fast, one slow
thread_fast = threading.Thread(target=parallel_booking_worker, args=(driver, 0, top_event, 2, config, select_vehicle_and_checkout, winner_info))
thread_slow = threading.Thread(target=parallel_booking_worker, args=(driver, 1, top_event, 5, config, select_vehicle_and_checkout, winner_info))

log("[MAIN] Starting parallel booking threads.")
thread_fast.start()
thread_slow.start()

thread_fast.join()
thread_slow.join()

log(f"[MAIN] Booking attempt finished. Winner: THREAD-{winner_info.get('winner')}. Closing browser in 5 seconds...")
time.sleep(5)
driver.quit()