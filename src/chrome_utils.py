import os
import sys
import shutil


def find_chrome_path():
    """
    Automatically find the Chrome executable path based on the user's platform.
    Returns the path as a string, or None if not found.
    """
    if sys.platform.startswith('darwin'):
        chrome_path = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'
        if os.path.exists(chrome_path):
            return chrome_path
    elif sys.platform.startswith('win'):
        paths = [
            os.path.join(os.environ.get('PROGRAMFILES', ''), 'Google', 'Chrome', 'Application', 'chrome.exe'),
            os.path.join(os.environ.get('PROGRAMFILES(X86)', ''), 'Google', 'Chrome', 'Application', 'chrome.exe'),
            os.path.join(os.environ.get('LOCALAPPDATA', ''), 'Google', 'Chrome', 'Application', 'chrome.exe'),
        ]
        for path in paths:
            if os.path.exists(path):
                return path
    elif sys.platform.startswith('linux'):
        chrome_path = shutil.which('google-chrome') or shutil.which('chrome') or shutil.which('chromium')
        if chrome_path:
            return chrome_path
    return None


def get_default_chrome_user_data_dir():
    """
    Get the default Chrome user data directory for the current platform.
    Returns the path as a string, or None if not found.
    """
    if sys.platform.startswith('darwin'):
        return os.path.expanduser('~/Library/Application Support/Google/Chrome')
    elif sys.platform.startswith('win'):
        return os.path.join(os.environ.get('LOCALAPPDATA', ''), 'Google', 'Chrome', 'User Data')
    elif sys.platform.startswith('linux'):
        return os.path.expanduser('~/.config/google-chrome')
    return None

def list_chrome_profiles(user_data_dir):
    """
    List available Chrome profile directories in the given user data directory.
    Returns a list of profile directory names (e.g., ['Default', 'Profile 1', ...]).
    """
    if not user_data_dir or not os.path.isdir(user_data_dir):
        return []
    profiles = []
    for entry in os.listdir(user_data_dir):
        entry_path = os.path.join(user_data_dir, entry)
        if os.path.isdir(entry_path) and (entry == 'Default' or entry.startswith('Profile')):
            profiles.append(entry)
    return profiles 