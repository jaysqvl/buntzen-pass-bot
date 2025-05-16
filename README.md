# Buntzen Pass Bot

This project automates the process of checking and reserving day passes for Buntzen Lake using Selenium and undetected-chromedriver. It can be scheduled to run at specific times and days, and uses NTP to ensure accurate timing.

## Features
- Automatically checks for All Day, Morning, or Afternoon passes
- Adds available passes to cart and proceeds to checkout
- Uses your existing Chrome profile for authentication
- Supports scheduling for specific days and times
- Uses NTP (Network Time Protocol) for accurate time

## Requirements
- Python 3.7+
- Google Chrome installed
- Logged into yodelportal (pass the 2FA)

### Python Packages
- undetected-chromedriver
- selenium
- fake_useragent
- python-dotenv
- ntplib

Install requirements with:
```bash
pip install -r requirements.txt
```

## Setup
1. **Clone this repository**
2. **Create a `.env` file** in the project root (or copy `.env.example`):

```
USER_DATA_DIR=/path/to/your/chrome/user/data
PROFILE_DIRECTORY=Default
VEHICLE_OPTION=Car
ALL_DAY_PASS_URL=https://your-all-day-pass-url.com
HALF_DAY_PASS_URL=https://your-half-day-pass-url.com
SCHEDULE=false
WAKEUP_TIME=06:55
START_TIME=07:00
DAY_OF_WEEK=Monday
CHECK_ALL_DAY=true
CHECK_MORNING=false
CHECK_AFTERNOON=false
```
- Adjust the values to match your setup and preferences.
- `USER_DATA_DIR` is usually something like `/home/youruser/.config/google-chrome` on Linux.
- `PROFILE_DIRECTORY` is typically `Default` or `Profile 1`.
- `VEHICLE_OPTION` should match the value attribute for your vehicle in the dropdown.
- Set `SCHEDULE` to `true` to enable scheduled runs.

3. **Ensure your Chrome profile is set up and logged in** (if the site requires authentication).

## Usage
Run the bot with:
```bash
python run.py
```

- If scheduling is enabled, the script will wait until the specified day and time before starting.
- The script will check for available passes and attempt to reserve one according to your settings.

## Troubleshooting
- **NTP errors:** If the script cannot reach the NTP server, it will fall back to your system clock and print a warning.
- **Chrome profile issues:** Make sure the `USER_DATA_DIR` and `PROFILE_DIRECTORY` are correct and not in use by another Chrome process.
- **Element not found errors:** The website layout may have changed. Update the XPaths in the script if needed.
- **Permissions:** Ensure you have permission to access your Chrome user data directory.

## Disclaimer
This script is for educational purposes only. Use responsibly and respect the terms of service of the website you are automating.
