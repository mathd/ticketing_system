#!/usr/bin/env python3
"""Browser verification for checkout against an already-running local stack."""

import os
from pathlib import Path

from playwright.sync_api import sync_playwright


base = os.environ.get("CHECKOUT_BASE_URL", "http://localhost:28080")
event_id = os.environ["CHECKOUT_EVENT_ID"]
evidence = Path("docs/verification/checkout")
evidence.mkdir(parents=True, exist_ok=True)
ticket_evidence = Path("docs/verification/ticket-delivery")
ticket_evidence.mkdir(parents=True, exist_ok=True)

with sync_playwright() as playwright:
    browser = playwright.chromium.launch(headless=True)
    page = browser.new_page(viewport={"width": 1280, "height": 900})
    page.goto(f"{base}/en/events/{event_id}")
    page.wait_for_load_state("networkidle")

    page.get_by_role("button", name="Reserve").click()
    page.locator(".checkout-form").wait_for()
    page.locator(".checkout-form button").click()
    page.get_by_text("Order confirmed").wait_for()
    page.screenshot(path=str(evidence / "checkout-success.png"), full_page=True)
    page.get_by_role("link", name="View my tickets").click()
    page.get_by_role("heading", name="My tickets").wait_for()
    page.get_by_role("img", name="Ticket QR code").wait_for()
    page.screenshot(path=str(ticket_evidence / "guest-ticket-qr.png"), full_page=True)

    page.goto(f"{base}/en/events/{event_id}")
    page.wait_for_load_state("networkidle")
    page.get_by_role("button", name="Reserve").click()
    page.locator(".checkout-form").wait_for()
    page.get_by_label("Fake payment").select_option("fake-decline")
    page.locator(".checkout-form button").click()
    page.get_by_text("Payment declined — try again").wait_for()
    page.get_by_role("button", name="Reserve").wait_for()
    page.screenshot(path=str(evidence / "checkout-declined-retriable.png"), full_page=True)
    browser.close()
