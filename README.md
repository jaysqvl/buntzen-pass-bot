# Buntzen Pass Bot (Beta)

> **Empowering everyone to enjoy BC's beautiful lakes and parks—no matter their tech skills.**

This project's purpose is to make it possible for anyone—including the technologically disadvantaged, like my parents—to get fair access to the beautiful lakes and parks of BC, such as Buntzen Lake. With the rise of online booking systems that sell out instantly, it's become nearly impossible for many people to secure a spot without technical help. My goal is to eventually provide a simple web UI so that anyone can set up what they want and book away—no coding or command line required. Buntzen has been my family's favourite lake for decades, and I want to make sure everyone has a fair chance to enjoy it, not just those with the fastest internet and reflexes.

---

## 🚀 Quick Start (complicated for now I know, docker self-hosted eventually)

1. **Clone this repository:**
   ```bash
   git clone https://github.com/yourusername/buntzen-pass-bot.git
   cd buntzen-pass-bot
   ```

2. **Create and activate a virtual environment:**
   ```bash
   python3 -m venv venv
   source venv/bin/activate
   ```

3. **Install dependencies:**
   ```bash
   pip install -r requirements.txt
   ```

4. **Copy the example config and edit it:**
   ```bash
   cp .env_example .env
   ```
   - Open `.env` in a text editor and fill in your preferences (see below for details).

5. **Run the bot:**
   ```bash
   python run.py
   ```

   - The bot will open Chrome. Log in to the booking site if prompted, then press Enter in your terminal to continue.

---

## Features
- Automatically checks for All Day, Morning, or Afternoon passes
- Adds available passes to cart and proceeds to checkout
- Uses your existing Chrome profile for authentication
- Supports precise scheduling for pass releases (with fast/slow polling)
- Uses NTP (Network Time Protocol) for accurate time
- Configurable via `.env` file
- **Future:** Simple web UI for non-technical users

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
