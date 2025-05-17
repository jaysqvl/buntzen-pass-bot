from selenium.common.exceptions import TimeoutException, NoSuchElementException
import time

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
                print("[DEBUG] Checking All Day Pass...")
                try:
                    all_day_pass = driver.find_element_by_xpath("//*[contains(text(), 'All-day Pass (8 a.m. to 8:00 p.m.)')]")
                    print("[DEBUG] Found All Day Pass element.")
                    availability_status = all_day_pass.find_element_by_xpath("../..//*[contains(text(), 'Available')]")
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