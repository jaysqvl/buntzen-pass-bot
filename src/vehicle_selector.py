from selenium.webdriver.common.by import By
from selenium.webdriver.support.ui import WebDriverWait, Select
from selenium.webdriver.support import expected_conditions as EC
from selenium.common.exceptions import TimeoutException, NoSuchElementException


def select_vehicle_and_checkout(driver, pass_element, vehicle_keyword):
    """
    Selects the vehicle whose name contains the given keyword (case-insensitive) and proceeds to checkout.
    Returns True if successful, False if no vehicle is found.
    """
    # Find and click the vehicle dropdown
    vehicle_dropdown = pass_element.find_element(By.XPATH, ".//select")
    vehicle_dropdown.click()

    # Select the vehicle by keyword
    select = Select(vehicle_dropdown)
    found = False
    for option in select.options:
        if vehicle_keyword and vehicle_keyword.lower() in option.text.lower():
            select.select_by_visible_text(option.text)
            found = True
            print(f"Selected vehicle: {option.text}")
            break
    if not found:
        print(f"Error: No vehicle found containing keyword '{vehicle_keyword}'. Please check your .env and try again.")
        return False

    # Click the "Add To Cart" button
    add_to_cart_button = pass_element.find_element(By.XPATH, ".//a[contains(text(), 'Add To Cart')]")
    add_to_cart_button.click()

    # Wait for the "Checkout" button and click it
    checkout_button = WebDriverWait(driver, 120).until(
        EC.element_to_be_clickable((By.XPATH, "//a[contains(text(), 'Checkout')]")
    ))
    checkout_button.click()

    # Wait for the "Yes" button to confirm checkout and click it
    yes_button = WebDriverWait(driver, 120).until(
        EC.element_to_be_clickable((By.XPATH, "//a[contains(text(), 'Yes')]")
    ))
    yes_button.click()

    print("Checkout confirmed successfully!")
    return True 