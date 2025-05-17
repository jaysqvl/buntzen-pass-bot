import os
from src.chrome_utils import get_default_chrome_user_data_dir

def load_config():
    """
    Load configuration from environment variables and apply override logic.
    Returns a dictionary with all relevant config values.
    """
    config = {}
    config['USER_DATA_DIR_OVERRIDE_ENABLED'] = os.getenv("USER_DATA_DIR_OVERRIDE_ENABLED", "false").lower() == "true"
    config['USER_DATA_DIR_OVERRIDE'] = os.getenv("USER_DATA_DIR_OVERRIDE")
    if config['USER_DATA_DIR_OVERRIDE_ENABLED'] and config['USER_DATA_DIR_OVERRIDE']:
        config['USER_DATA_DIR'] = config['USER_DATA_DIR_OVERRIDE']
    else:
        config['USER_DATA_DIR'] = get_default_chrome_user_data_dir()
    config['PROFILE_DIRECTORY'] = os.getenv("PROFILE_DIRECTORY")
    config['VEHICLE_KEYWORD'] = os.getenv("VEHICLE_KEYWORD")
    config['ALL_DAY_PASS_URL'] = os.getenv("ALL_DAY_PASS_URL")
    config['HALF_DAY_PASS_URL'] = os.getenv("HALF_DAY_PASS_URL")
    config['SCHEDULE'] = os.getenv("SCHEDULE", "false").lower() == "true"
    config['WAKEUP_TIME'] = os.getenv("WAKEUP_TIME")
    config['START_TIME'] = os.getenv("START_TIME")
    config['DAY_OF_WEEK'] = os.getenv("DAY_OF_WEEK").capitalize() if os.getenv("DAY_OF_WEEK") else None
    config['CHECK_ALL_DAY'] = os.getenv("CHECK_ALL_DAY", "false").lower() == "true"
    config['CHECK_MORNING'] = os.getenv("CHECK_MORNING", "false").lower() == "true"
    config['CHECK_AFTERNOON'] = os.getenv("CHECK_AFTERNOON", "false").lower() == "true"
    return config