from selenium import webdriver
from selenium.webdriver.chrome.service import Service
from selenium.webdriver.common.by import By
from selenium.webdriver.common.action_chains import ActionChains
from selenium.webdriver.chrome.options import Options
import time
import random

# --------- SETUP ---------

# Your specific ChromeDriver path
chrome_driver_path = r"C:\Users\Parth Aditya\path\chromedriver-win64\chromedriver.exe"

# Optional user-agent rotation for realism
user_agents = [
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/122.0.0.0 Safari/537.36",
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/15.1 Safari/605.1.15",
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36"
]

options = Options()
options.add_argument(f"user-agent={random.choice(user_agents)}")

# Start Chrome browser
service = Service(executable_path=chrome_driver_path)
driver = webdriver.Chrome(service=service, options=options)
actions = ActionChains(driver)

# --------- GO TO PYTHON.ORG ---------

driver.get("https://www.python.org")
driver.maximize_window()
time.sleep(random.uniform(2, 4))

# --------- RANDOMIZED PAGE VISITS ---------

# Example link texts that exist on python.org homepage
pages_to_visit = ["Downloads", "Documentation", "Community", "Success Stories", "News"]
random.shuffle(pages_to_visit)  # Shuffle visit order

def visit_link_by_text(link_text):
    try:
        link = driver.find_element(By.LINK_TEXT, link_text)

        # Slight random movement before clicking
        offset_x = random.randint(-10, 10)
        offset_y = random.randint(-10, 10)
        actions.move_to_element_with_offset(link, offset_x, offset_y).perform()
        time.sleep(random.uniform(1, 2))

        link.click()
        print(f"Visited {link_text}")
        time.sleep(random.uniform(2, 5))

        driver.back()
        time.sleep(random.uniform(2, 4))
    except Exception as e:
        print(f"Could not visit {link_text}: {e}")

# Visit a few of the shuffled pages
for page in pages_to_visit[:random.randint(2, 5)]:
    visit_link_by_text(page)

# --------- SCROLL SIMULATION ---------

# Scroll in chunks to simulate reading
for _ in range(random.randint(2, 4)):
    scroll_amount = random.randint(300, 800)
    driver.execute_script(f"window.scrollBy(0, {scroll_amount});")
    time.sleep(random.uniform(1, 2))

# --------- DONE ---------

driver.quit()
