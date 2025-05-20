import os

def load_config():
    """
    Load configuration from environment variables and apply default logic.
    Returns a dictionary with all relevant config values.
    """
    config = {}
    user_data_dir = os.getenv("USER_DATA_DIR")
    if not user_data_dir or user_data_dir.strip() == "":
        user_data_dir = os.path.join(os.path.dirname(os.path.dirname(__file__)), "chrome-profile")
    config['USER_DATA_DIR'] = user_data_dir
    config['TARGET_DATE'] = os.getenv("TARGET_DATE")
    config['SCHEDULE'] = os.getenv("SCHEDULE", "false").lower() == "true"
    config['SLOW_POLL_UNTIL'] = os.getenv("SLOW_POLL_UNTIL")
    config['START_TIME'] = os.getenv("START_TIME")
    config['VEHICLE_KEYWORD'] = os.getenv("VEHICLE_KEYWORD")
    config['ALL_DAY_PASS_URL'] = os.getenv("ALL_DAY_PASS_URL")
    config['HALF_DAY_PASS_URL'] = os.getenv("HALF_DAY_PASS_URL")
    config['CHECK_ALL_DAY'] = os.getenv("CHECK_ALL_DAY", "false").lower() == "true"
    config['CHECK_MORNING'] = os.getenv("CHECK_MORNING", "false").lower() == "true"
    config['CHECK_AFTERNOON'] = os.getenv("CHECK_AFTERNOON", "false").lower() == "true"
    return config