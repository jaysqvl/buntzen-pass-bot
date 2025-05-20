from selenium.webdriver.common.by import By
from selenium.webdriver.support.ui import WebDriverWait, Select
from selenium.webdriver.support import expected_conditions as EC
from selenium.common.exceptions import TimeoutException, NoSuchElementException
import time
from .selenium_utils import robust_find_and_act


def select_vehicle_and_checkout(driver, pass_element, vehicle_keyword):
    """
    Selects the vehicle whose name contains the given keyword (case-insensitive) and proceeds to checkout.
    Returns True if successful, False if no vehicle is found.
    """
    # Click the smart vehicle selector to open the popup
    try:
        smart_selector = robust_find_and_act(driver, By.CSS_SELECTOR, ".smartSelectCustom", action=lambda el: el.click(), wait_condition='clickable', timeout=10, retries=10)
        print("[DEBUG] Clicking smart vehicle selector to open popup...")
    except Exception as e:
        print(f"[ERROR] Could not find or click smart vehicle selector: {e}")
        return False

    # Wait for the popup to appear
    try:
        popup = robust_find_and_act(driver, By.CSS_SELECTOR, ".popup.smart-select-popup.modal-in", wait_condition='visible', timeout=10, retries=10)
        print("[DEBUG] Vehicle selection popup appeared.")
    except TimeoutException:
        print("[ERROR] Vehicle selection popup did not appear.")
        return False

    # Find all vehicle radio options in the popup
    vehicle_found = False
    try:
        vehicle_labels = popup.find_elements(By.CSS_SELECTOR, "label.item-radio")
        for label in vehicle_labels:
            title_div = label.find_element(By.CSS_SELECTOR, ".item-title")
            if vehicle_keyword and vehicle_keyword.lower() in title_div.text.lower():
                print(f"[DEBUG] Selecting vehicle: {title_div.text}")
                robust_find_and_act(driver, By.CSS_SELECTOR, f"label.item-radio:nth-child({vehicle_labels.index(label)+1})", action=lambda el: el.click(), wait_condition='clickable', timeout=5, retries=5)
                vehicle_found = True
                break
        if not vehicle_found:
            print(f"Error: No vehicle found containing keyword '{vehicle_keyword}'. Please check your .env and try again.")
            return False
    except Exception as e:
        print(f"[ERROR] Could not select vehicle in popup: {e}")
        return False

    # Close the popup (simulate clicking the close button or outside the popup)
    try:
        close_btn = robust_find_and_act(driver, By.CSS_SELECTOR, ".link.popup-close", action=lambda el: el.click(), wait_condition='clickable', timeout=5, retries=5)
        print("[DEBUG] Closed vehicle selection popup.")
    except Exception as e:
        print(f"[WARN] Could not find or click popup close button: {e}. Trying to continue.")
        # Sometimes popup closes automatically after selection
        pass

    # Wait a moment for the selection to register
    time.sleep(0.5)

    # Click the "Add To Cart" button
    try:
        add_to_cart_button = robust_find_and_act(driver, By.XPATH, ".//a[contains(text(), 'Add To Cart')]", action=lambda el: el.click(), wait_condition='clickable', timeout=10, retries=10)
    except Exception as e:
        print(f"[ERROR] Could not find or click Add To Cart button: {e}")
        return False

    # Wait for the checkout page and click the main checkout button
    try:
        checkout_button = robust_find_and_act(driver, By.ID, "checkOutButton", action=lambda el: el.click(), wait_condition='clickable', timeout=30, retries=10)
        print("[DEBUG] Clicking main checkout button...")
    except Exception as e:
        print(f"[ERROR] Could not find or click main checkout button: {e}")
        return False

    # Wait for the "Yes" button to confirm checkout and click it
    try:
        yes_button = robust_find_and_act(driver, By.XPATH, "//a[contains(text(), 'Yes')]", action=lambda el: el.click(), wait_condition='clickable', timeout=120, retries=10)
    except Exception as e:
        print(f"[ERROR] Could not find or click Yes button: {e}")
        return False

    print("Checkout confirmed successfully!")
    return True 