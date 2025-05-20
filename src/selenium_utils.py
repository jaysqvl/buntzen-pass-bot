from selenium.webdriver.support.ui import WebDriverWait
from selenium.webdriver.support import expected_conditions as EC
from selenium.common.exceptions import (
    TimeoutException, StaleElementReferenceException, ElementClickInterceptedException, NoSuchElementException
)
import time


def robust_find_and_act(driver, by, selector, action=None, timeout=10, retries=5, poll_frequency=0.2, wait_condition='clickable', action_args=None):
    """
    Robustly find an element and perform an action (e.g., click), retrying on transient errors.
    - by: Selenium By selector
    - selector: selector string
    - action: function to call on the element (e.g., lambda el: el.click()), or None to just return the element
    - timeout: max seconds to wait for the element to be ready each try
    - retries: number of retries for the action
    - poll_frequency: polling interval in seconds
    - wait_condition: 'clickable', 'visible', or 'present'
    - action_args: tuple of arguments to pass to the action
    Returns the element if successful, or raises the last exception if not.
    """
    wait_map = {
        'clickable': EC.element_to_be_clickable,
        'visible': EC.visibility_of_element_located,
        'present': EC.presence_of_element_located,
    }
    last_exception = None
    for attempt in range(1, retries + 1):
        try:
            wait = WebDriverWait(driver, timeout, poll_frequency=poll_frequency)
            condition = wait_map.get(wait_condition, EC.element_to_be_clickable)
            element = wait.until(condition((by, selector)))
            if action:
                if action_args:
                    action(element, *action_args)
                else:
                    action(element)
            return element
        except (StaleElementReferenceException, ElementClickInterceptedException, TimeoutException, NoSuchElementException) as e:
            print(f"[WARN] Attempt {attempt}/{retries} for selector '{selector}' failed: {e}")
            last_exception = e
            time.sleep(poll_frequency)
        except Exception as e:
            print(f"[ERROR] Unexpected error on attempt {attempt}/{retries} for selector '{selector}': {e}")
            last_exception = e
            break
    print(f"[ERROR] All {retries} attempts failed for selector '{selector}'. Raising last exception.")
    raise last_exception 