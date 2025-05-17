import undetected_chromedriver as uc
from fake_useragent import UserAgent
from dotenv import load_dotenv
import sys
import time

from src.env_utils import load_config
from src.chrome_utils import find_chrome_path
from src.vehicle_selector import select_vehicle_and_checkout
from src.booking import run_booking_flow
from src.scheduler import get_ntp_time, get_seconds_until, get_days_until

# Load environment variables from .env file
load_dotenv(override=True)

# Load config
config = load_config()

# Only use scheduling if SCHEDULE is true and it is the correct day of the week
if config['SCHEDULE']:
    # Calculate days until the specified day of the week
    days_until_target = get_days_until(config['DAY_OF_WEEK'])

    if days_until_target > 0:
        # Sleep until the next occurrence of the specified day
        seconds_until_target_day = days_until_target * 24 * 60 * 60
        print(f"Script will wake up on {config['DAY_OF_WEEK']}, which is in {seconds_until_target_day} seconds.")
        time.sleep(seconds_until_target_day)

    # Sleep until the wake-up time on the target day
    time_until_wakeup = get_seconds_until(config['WAKEUP_TIME'])
    print(f"Script will wake up at {config['WAKEUP_TIME']} on {config['DAY_OF_WEEK']}, which is in {time_until_wakeup} seconds.")
    time.sleep(time_until_wakeup)

    # Start a loop that checks the time every second until it's exactly the start time
    print(f"Waiting for {config['START_TIME']}...")
    while True:
        now = get_ntp_time()
        if now.strftime("%H:%M") == config['START_TIME']:
            print(f"It's {config['START_TIME']} on {config['DAY_OF_WEEK']}! Starting the script.")
            break
        time.sleep(1)

# Generate a random user-agent
ua = UserAgent()
user_agent = ua.random

# Set up Chrome options with the random user-agent and your existing profile
driver_options = uc.ChromeOptions()
driver_options.add_argument('--no-sandbox')
driver_options.add_argument('--disable-dev-shm-usage')
driver_options.add_argument('--enable-javascript')
driver_options.add_argument(f'--user-agent={user_agent}')
driver_options.add_argument(f'--user-data-dir={config["USER_DATA_DIR"]}')
driver_options.add_argument(f'--profile-directory={config["PROFILE_DIRECTORY"]}')
print(f"Chrome will use user data dir: {config['USER_DATA_DIR']}")
print(f"Chrome will use profile directory: {config['PROFILE_DIRECTORY']}")

# Find and set the Chrome binary location automatically
# chrome_path = find_chrome_path()
# if chrome_path:
#     driver_options.binary_location = chrome_path
# else:
#     print("Could not find Chrome. Please install it or enable DIR override in your .env and input a path.")
#     sys.exit(1)

print("Loaded config:", config)
# print(f"Using Chrome binary at: {chrome_path}")
print(f"User agent: {user_agent}")
print("Initializing Chrome driver...")
driver = uc.Chrome(options=driver_options, use_subprocess=True)
print("Chrome driver initialized. Starting booking flow...")

# Run the main booking flow
run_booking_flow(driver, config, select_vehicle_and_checkout)