# Buntzen Pass Bot

This project automates the process of checking and reserving day passes for Buntzen Lake using Selenium and undetected-chromedriver. It supports precise scheduling for pass releases, uses your Chrome profile for authentication, and leverages NTP for accurate timing.

## Features
- Automatically checks for All Day, Morning, or Afternoon passes
- Adds available passes to cart and proceeds to checkout
- Uses your existing Chrome profile for authentication
- Supports scheduling for specific dates and times (with fast/slow polling)
- Uses NTP (Network Time Protocol) for accurate time
- Configurable via `.env` file

## Requirements
- Python 3.7+
- Google Chrome installed
- Logged into yodelportal (pass the 2FA)

### Python Packages
- selenium
- undetected-chromedriver
- fake-useragent
- ntplib
- dotenv
- setuptools
- pytz

Install requirements with:
```bash
pip install -r requirements.txt
```

## Setup
1. **Clone this repository**
2. **Create and activate a virtual environment**

On Linux/macOS:
```bash
python3 -m venv venv
source venv/bin/activate
```

On Windows:
```cmd
python -m venv venv
venv\Scripts\activate
```

3. **Create a `.env` file** in the project root (or copy `.env_example`):

```
# --- Scheduling Configuration ---
TARGET_DATE=2024-06-18
SCHEDULE=true
USER_DATA_DIR=chrome-profile

# --- Booking Configuration ---
VEHICLE_KEYWORD=Tesla
ALL_DAY_PASS_URL=https://your-all-day-pass-url.com
HALF_DAY_PASS_URL=https://your-half-day-pass-url.com

# --- Optional: Polling Configuration ---
SLOW_POLL_UNTIL=06:59
START_TIME=07:00
DAY_OF_WEEK=Monday

# --- Pass Type Selection ---
CHECK_ALL_DAY=true
CHECK_MORNING=false
CHECK_AFTERNOON=false
```
- Adjust the values to match your setup and preferences.
- `USER_DATA_DIR` defaults to `chrome-profile` in this repo if left blank.
- `VEHICLE_KEYWORD` should be a unique keyword or phrase to identify your vehicle in the dropdown (e.g., 'Tesla', 'MV763F').
- Set `SCHEDULE` to `true` to enable scheduled runs for a specific `TARGET_DATE`.
- `SLOW_POLL_UNTIL` and `START_TIME` control the polling speed before booking opens.

4. **Ensure your Chrome profile is set up and logged in** (if the site requires authentication).

## Usage
Run the bot with:
```bash
python run.py
```

- If scheduling is enabled, the script will wait for the correct release window for your `TARGET_DATE` before starting the booking flow.
- The script will check for available passes and attempt to reserve one according to your settings.

## Troubleshooting
- **NTP errors:** If the script cannot reach the NTP server, it will fall back to your system clock and print a warning.
- **Chrome profile issues:** Make sure the `USER_DATA_DIR` is correct and not in use by another Chrome process.
- **Element not found errors:** The website layout may have changed. Update the XPaths in the script if needed.
- **Permissions:** Ensure you have permission to access your Chrome user data directory.
- **Vehicle not found:** Ensure `VEHICLE_KEYWORD` matches a unique part of your vehicle's name in the dropdown.

## Disclaimer
This script is for educational purposes only. Use responsibly and respect the terms of service of the website you are automating.
