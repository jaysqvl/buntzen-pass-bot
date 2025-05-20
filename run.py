"""
Entry point for Buntzen Pass Bot.
Sets up environment, loads config, and runs the main booking controller.
"""
import undetected_chromedriver as uc
from fake_useragent import UserAgent
from dotenv import load_dotenv
import time
import os
from datetime import datetime, timedelta
import pytz
import logging

from src.env_utils import load_config
from src.vehicle_selector import select_vehicle_and_checkout
from src.booking import main_booking_controller
from src.scheduler import get_ntp_time, get_seconds_until, get_days_until
from src.chrome_utils import open_new_tab, switch_to_tab

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("buntzen_pass_bot.run")

def log(msg):
    logger.info(msg)

if __name__ == "__main__":
    load_dotenv(override=True)
    config = load_config()
    # --- Scheduling for Pass Release ---
    if config.get('SCHEDULE') and config.get('TARGET_DATE'):
        target_date = datetime.strptime(config['TARGET_DATE'], "%Y-%m-%d")
        release_date = target_date - timedelta(days=1)
        pst = pytz.timezone('US/Pacific')
        slow_poll_until_time = datetime.strptime(config['SLOW_POLL_UNTIL'], "%H:%M").time()
        start_time = datetime.strptime(config['START_TIME'], "%H:%M").time()
        slow_poll_until_pst = pst.localize(datetime.combine(release_date, slow_poll_until_time))
        start_time_pst = pst.localize(datetime.combine(release_date, start_time))
        slow_poll_until_utc = slow_poll_until_pst.astimezone(pytz.utc)
        start_time_utc = start_time_pst.astimezone(pytz.utc)
        logger.info(f"Target pass date: {config['TARGET_DATE']}")
        logger.info(f"Will slow poll until: {slow_poll_until_pst.strftime('%Y-%m-%d %H:%M:%S %Z')} (PST)")
        logger.info(f"Will start booking flow at: {start_time_pst.strftime('%Y-%m-%d %H:%M:%S %Z')} (PST)")
        while True:
            now_utc = get_ntp_time().replace(tzinfo=pytz.utc)
            if now_utc >= slow_poll_until_utc:
                break
            seconds_until_slow_poll = (slow_poll_until_utc - now_utc).total_seconds()
            logger.info(f"[Slow Poll] Waiting for fast poll window... {int(seconds_until_slow_poll)} seconds remaining.")
            time.sleep(1)
        while True:
            now_utc = get_ntp_time().replace(tzinfo=pytz.utc)
            if now_utc >= start_time_utc:
                logger.info(f"It's time! {now_utc.strftime('%Y-%m-%d %H:%M:%S %Z')} (UTC)")
                break
            seconds_until_start = (start_time_utc - now_utc).total_seconds()
            if int(seconds_until_start * 100) % 10 == 0:
                logger.info(f"[Fast Poll] Waiting for booking start... {seconds_until_start:.2f} seconds remaining.")
            time.sleep(0.05)
    # Generate a random user-agent
    ua = UserAgent()
    user_agent = ua.random
    driver_options = uc.ChromeOptions()
    driver_options.add_argument('--no-sandbox')
    driver_options.add_argument('--disable-dev-shm-usage')
    driver_options.add_argument("--disable-gpu")
    driver_options.add_argument('--enable-javascript')
    driver_options.add_argument(f'--user-agent={user_agent}')
    os.makedirs(config["USER_DATA_DIR"], exist_ok=True)
    driver_options.add_argument(f'--user-data-dir={config["USER_DATA_DIR"]}')
    logger.info("Loaded config: %s", config)
    logger.info(f"User agent: {user_agent}")
    logger.info(f"Chrome will use user data dir: {config['USER_DATA_DIR']}")
    logger.info("Initializing Chrome driver...")
    driver = uc.Chrome(options=driver_options, use_subprocess=True)
    logger.info("Chrome driver initialized. Opening Browser...")
    logger.info("Please log in to the Buntzen website in the opened browser window.")
    input("Once you have successfully logged in, press Enter here to continue...")
    main_booking_controller(driver, config, select_vehicle_and_checkout)
    logger.info("All booking attempts finished. Closing browser in 5 seconds...")
    time.sleep(5)
    driver.quit()