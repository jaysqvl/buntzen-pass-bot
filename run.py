import undetected_chromedriver as uc
from fake_useragent import UserAgent
from dotenv import load_dotenv
import time
import os
from datetime import datetime, timedelta, time as dt_time
import pytz

from src.env_utils import load_config
from src.vehicle_selector import select_vehicle_and_checkout
from src.booking import run_booking_flow
from src.scheduler import get_ntp_time, get_seconds_until, get_days_until

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
print("Chrome driver initialized. Starting booking flow...")

# Run the main booking flow
run_booking_flow(driver, config, select_vehicle_and_checkout)