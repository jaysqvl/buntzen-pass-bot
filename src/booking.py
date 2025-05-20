from selenium.common.exceptions import TimeoutException, NoSuchElementException
from selenium.webdriver.common.by import By
from selenium.webdriver.support.ui import WebDriverWait
from selenium.webdriver.support import expected_conditions as EC
import time
from .selenium_utils import robust_find_and_act

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
            # Check for All Day Passes
            if config['CHECK_ALL_DAY']:
                print(f"[DEBUG] Opening All Day Pass URL: {config['ALL_DAY_PASS_URL']}")
                driver.get(config['ALL_DAY_PASS_URL'])
                print("[DEBUG] Waiting for date buttons to load...")
                try:
                    # --- Wait for date buttons or reload if missing ---
                    reload_attempts = 0
                    while True:
                        try:
                            robust_find_and_act(driver, By.CSS_SELECTOR, ".datelist button.date", wait_condition='present', timeout=5, retries=2)
                            break  # Found date buttons, proceed
                        except Exception as e:
                            reload_attempts += 1
                            print(f"[WARN] Date buttons not found after 5s, reloading page (attempt {reload_attempts})...")
                            driver.refresh()
                            time.sleep(1)  # Give the browser a moment to reload
                    # Find all date buttons
                    date_buttons = driver.find_elements(By.CSS_SELECTOR, ".datelist button.date")
                    found = False
                    target_day = str(int(config['TARGET_DATE'].split('-')[2]))  # Remove leading zero
                    for btn in date_buttons:
                        if btn.text.strip() == target_day:
                            # If not already active, click it
                            if 'active' not in btn.get_attribute('class'):
                                print(f"[DEBUG] Clicking date button for {target_day}")
                                robust_find_and_act(driver, By.CSS_SELECTOR, f".datelist button.date:nth-child({date_buttons.index(btn)+1})", action=lambda el: el.click(), wait_condition='clickable', timeout=5, retries=5)
                                # Wait for the button to become active
                                WebDriverWait(driver, 5).until(
                                    lambda d: 'active' in btn.get_attribute('class')
                                )
                            else:
                                print(f"[DEBUG] Date button for {target_day} is already active.")
                            found = True
                            break
                    if not found:
                        print(f"[DEBUG] Date button for {target_day} not found. Retrying...")
                        time.sleep(2)
                        continue  # Retry loop
                    print(f"[DEBUG] Correct date button for {target_day} is now active.")
                    # --- End date selection ---

                    print("[DEBUG] Checking All Day Pass...")
                    all_day_pass = robust_find_and_act(driver, By.XPATH, "//*[contains(text(), 'All-day Pass (8 a.m. to 8:00 p.m.)')]", wait_condition='visible', timeout=10, retries=10)
                    print("[DEBUG] Found All Day Pass element.")
                    availability_status = robust_find_and_act(all_day_pass, By.XPATH, "../..//*[contains(text(), 'Available')]", wait_condition='visible', timeout=5, retries=5)
                    print("[DEBUG] Found availability status for All Day Pass.")
                    if availability_status:
                        print("All Day Pass is available! Proceeding to add to cart...")
                        if select_vehicle_and_checkout(driver, all_day_pass, config['VEHICLE_KEYWORD']):
                            print("[DEBUG] Successfully checked out All Day Pass.")
                            break
                except (TimeoutException, NoSuchElementException) as e:
                    print(f"[DEBUG] All Day Pass is sold out or not loaded yet. Exception: {e}. Moving to next check.")

            # Check for Morning and Afternoon Passes
            if config['CHECK_MORNING'] or config['CHECK_AFTERNOON']:
                print(f"[DEBUG] Opening Half Day Pass URL: {config['HALF_DAY_PASS_URL']}")
                driver.get(config['HALF_DAY_PASS_URL'])
                if config['CHECK_MORNING']:
                    print("[DEBUG] Checking Morning Pass...")
                    try:
                        morning_pass = driver.find_element_by_xpath("//*[contains(text(), 'Morning Pass (8 a.m. to 2 p.m.)')]")
                        print("[DEBUG] Found Morning Pass element.")
                        availability_status = morning_pass.find_element_by_xpath("../..//*[contains(text(), 'Available')]")
                        print("[DEBUG] Found availability status for Morning Pass.")
                        if availability_status:
                            print("Morning Pass is available! Proceeding to add to cart...")
                            if select_vehicle_and_checkout(driver, morning_pass, config['VEHICLE_KEYWORD']):
                                print("[DEBUG] Successfully checked out Morning Pass.")
                                break
                    except (TimeoutException, NoSuchElementException) as e:
                        print(f"[DEBUG] Morning Pass is sold out or not loaded yet. Exception: {e}. Moving to next check.")

                if config['CHECK_AFTERNOON']:
                    print("[DEBUG] Checking Afternoon Pass...")
                    try:
                        afternoon_pass = driver.find_element_by_xpath("//*[contains(text(), 'Afternoon Pass (2 p.m. to 8:00 p.m.)')]")
                        print("[DEBUG] Found Afternoon Pass element.")
                        availability_status = afternoon_pass.find_element_by_xpath("../..//*[contains(text(), 'Available')]")
                        print("[DEBUG] Found availability status for Afternoon Pass.")
                        if availability_status:
                            print("Afternoon Pass is available! Proceeding to add to cart...")
                            if select_vehicle_and_checkout(driver, afternoon_pass, config['VEHICLE_KEYWORD']):
                                print("[DEBUG] Successfully checked out Afternoon Pass.")
                                break
                    except (TimeoutException, NoSuchElementException) as e:
                        print(f"[DEBUG] Afternoon Pass is sold out or not loaded yet. Exception: {e}.")

            print("[DEBUG] Sleeping for 5 seconds before retrying...")
            time.sleep(5)
    except Exception as e:
        print(f"[ERROR] An error occurred in booking flow: {e}")
    finally:
        print("[DEBUG] Booking flow finished. Closing driver in 5 seconds...")
        time.sleep(5)
        driver.quit() 