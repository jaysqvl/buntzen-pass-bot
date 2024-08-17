import undetected_chromedriver as uc
from selenium.webdriver.common.by import By
from selenium.webdriver.support.ui import WebDriverWait
from selenium.webdriver.support import expected_conditions as EC
from selenium.webdriver.support.ui import Select
from selenium.common.exceptions import TimeoutException, StaleElementReferenceException, NoSuchElementException
import time
from fake_useragent import UserAgent
from datetime import datetime, timedelta
from dotenv import load_dotenv
import os

# Load environment variables from .env file
load_dotenv()

# Get configuration from .env
USER_DATA_DIR = os.getenv("USER_DATA_DIR")
PROFILE_DIRECTORY = os.getenv("PROFILE_DIRECTORY")
VEHICLE_OPTION = os.getenv("VEHICLE_OPTION")
ALL_DAY_PASS_URL = os.getenv("ALL_DAY_PASS_URL")
HALF_DAY_PASS_URL = os.getenv("HALF_DAY_PASS_URL")
SCHEDULE = os.getenv("SCHEDULE", "false").lower() == "true"
WAKEUP_TIME = os.getenv("WAKEUP_TIME")
START_TIME = os.getenv("START_TIME")
DAY_OF_WEEK = os.getenv("DAY_OF_WEEK").capitalize()
CHECK_ALL_DAY = os.getenv("CHECK_ALL_DAY", "false").lower() == "true"
CHECK_MORNING = os.getenv("CHECK_MORNING", "false").lower() == "true"
CHECK_AFTERNOON = os.getenv("CHECK_AFTERNOON", "false").lower() == "true"

# Function to calculate sleep time until a specified time tomorrow
def get_seconds_until(target_time):
    now = datetime.now()
    target_time = datetime.strptime(target_time, "%H:%M").time()
    target_datetime = datetime.combine(now.date() + timedelta(days=1), target_time)
    return (target_datetime - now).total_seconds()

# Function to calculate days until the next occurrence of a specific day of the week
def get_days_until(day_name):
    now = datetime.now()
    today_weekday = now.weekday()  # Monday is 0 and Sunday is 6
    target_weekday = ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"].index(day_name)
    days_until = (target_weekday - today_weekday + 7) % 7
    return days_until or 7  # If today is the target day, schedule for next week

# Only use scheduling if SCHEDULE is true and it is the correct day of the week
if SCHEDULE:
    # Calculate days until the specified day of the week
    days_until_target = get_days_until(DAY_OF_WEEK)

    if days_until_target > 0:
        # Sleep until the next occurrence of the specified day
        seconds_until_target_day = days_until_target * 24 * 60 * 60
        print(f"Script will wake up on {DAY_OF_WEEK}, which is in {seconds_until_target_day} seconds.")
        time.sleep(seconds_until_target_day)

    # Sleep until the wake-up time on the target day
    time_until_wakeup = get_seconds_until(WAKEUP_TIME)
    print(f"Script will wake up at {WAKEUP_TIME} on {DAY_OF_WEEK}, which is in {time_until_wakeup} seconds.")
    time.sleep(time_until_wakeup)

    # Start a loop that checks the time every second until it's exactly the start time
    print(f"Waiting for {START_TIME}...")
    while True:
        now = datetime.now()
        if now.strftime("%H:%M") == START_TIME:
            print(f"It's {START_TIME} on {DAY_OF_WEEK}! Starting the script.")
            break
        time.sleep(1)

# Generate a random user-agent
ua = UserAgent()
user_agent = ua.random

# Set up Chrome options with the random user-agent and your existing profile
options = uc.ChromeOptions()
options.add_argument(f'user-agent={user_agent}')
options.add_argument(f"user-data-dir={USER_DATA_DIR}")
options.add_argument(f"profile-directory={PROFILE_DIRECTORY}")

# Initialize the undetected Chrome driver with options
driver = uc.Chrome(options=options)

try:
    # Retry logic: keep refreshing the page until any of the specified passes is available
    while True:
        # Check for All Day Passes
        if CHECK_ALL_DAY:
            driver.get(ALL_DAY_PASS_URL)
            print("Checking All Day Pass...")
            try:
                all_day_pass = driver.find_element(By.XPATH, "//*[contains(text(), 'All-day Pass (8 a.m. to 8:00 p.m.)')]")
                availability_status = all_day_pass.find_element(By.XPATH, "../..//*[contains(text(), 'Available')]")
                if availability_status:
                    print("All Day Pass is available! Proceeding to add to cart...")
                    # Select vehicle and add to cart
                    select_vehicle_and_checkout(driver, all_day_pass)
                    break  # Exit loop if successful
            except (TimeoutException, NoSuchElementException):
                print("All Day Pass is sold out or not loaded yet. Moving to next check.")

        # Check for Morning and Afternoon Passes
        if CHECK_MORNING or CHECK_AFTERNOON:
            driver.get(HALF_DAY_PASS_URL)
            if CHECK_MORNING:
                print("Checking Morning Pass...")
                try:
                    morning_pass = driver.find_element(By.XPATH, "//*[contains(text(), 'Morning Pass (8 a.m. to 2 p.m.)')]")
                    availability_status = morning_pass.find_element(By.XPATH, "../..//*[contains(text(), 'Available')]")
                    if availability_status:
                        print("Morning Pass is available! Proceeding to add to cart...")
                        # Select vehicle and add to cart
                        select_vehicle_and_checkout(driver, morning_pass)
                        break  # Exit loop if successful
                except (TimeoutException, NoSuchElementException):
                    print("Morning Pass is sold out or not loaded yet. Moving to next check.")

            if CHECK_AFTERNOON:
                print("Checking Afternoon Pass...")
                try:
                    afternoon_pass = driver.find_element(By.XPATH, "//*[contains(text(), 'Afternoon Pass (2 p.m. to 8:00 p.m.)')]")
                    availability_status = afternoon_pass.find_element(By.XPATH, "../..//*[contains(text(), 'Available')]")
                    if availability_status:
                        print("Afternoon Pass is available! Proceeding to add to cart...")
                        # Select vehicle and add to cart
                        select_vehicle_and_checkout(driver, afternoon_pass)
                        break  # Exit loop if successful
                except (TimeoutException, NoSuchElementException):
                    print("Afternoon Pass is sold out or not loaded yet.")

        time.sleep(5)  # Short delay before retrying

# Function to select vehicle and checkout
def select_vehicle_and_checkout(driver, pass_element):
    # Find and click the vehicle dropdown
    vehicle_dropdown = pass_element.find_element(By.XPATH, ".//select")
    vehicle_dropdown.click()

    # Select the vehicle by value
    select = Select(vehicle_dropdown)
    select.select_by_value(VEHICLE_OPTION)

    # Click the "Add To Cart" button
    add_to_cart_button = pass_element.find_element(By.XPATH, ".//a[contains(text(), 'Add To Cart')]")
    add_to_cart_button.click()

    # Wait for the "Checkout" button and click it
    checkout_button = WebDriverWait(driver, 120).until(
        EC.element_to_be_clickable((By.XPATH, "//a[contains(text(), 'Checkout')]"))
    )
    checkout_button.click()

    # Wait for the "Yes" button to confirm checkout and click it
    yes_button = WebDriverWait(driver, 120).until(
        EC.element_to_be_clickable((By.XPATH, "//a[contains(text(), 'Yes')]"))
    )
    yes_button.click()

    print("Checkout confirmed successfully!")

except Exception as e:
    print(f"An error occurred: {e}")
finally:
    # Close the driver after a short delay
    time.sleep(5)
    driver.quit()
