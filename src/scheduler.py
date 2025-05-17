from datetime import datetime, timedelta
import ntplib


def get_ntp_time():
    """
    Get the current time from an NTP server. Falls back to system time on failure.
    Returns a datetime object.
    """
    try:
        client = ntplib.NTPClient()
        response = client.request('pool.ntp.org', version=3, timeout=5)
        return datetime.fromtimestamp(response.tx_time)
    except Exception as e:
        print(f"Warning: Could not get NTP time, falling back to system time. Reason: {e}")
        return datetime.now()


def get_seconds_until(target_time):
    """
    Calculate seconds until a specified time tomorrow.
    target_time: string in '%H:%M' format.
    Returns seconds as a float.
    """
    now = get_ntp_time()
    target_time_obj = datetime.strptime(target_time, "%H:%M").time()
    target_datetime = datetime.combine(now.date() + timedelta(days=1), target_time_obj)
    return (target_datetime - now).total_seconds()


def get_days_until(day_name):
    """
    Calculate days until the next occurrence of a specific day of the week.
    day_name: string (e.g., 'Monday')
    Returns days as an int.
    """
    now = get_ntp_time()
    today_weekday = now.weekday()  # Monday is 0 and Sunday is 6
    target_weekday = ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"].index(day_name)
    days_until = (target_weekday - today_weekday + 7) % 7
    return days_until or 7  # If today is the target day, schedule for next week 