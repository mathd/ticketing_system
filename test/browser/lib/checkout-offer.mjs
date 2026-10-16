// The GA offer the checkout specs buy from (moved out of checkout.mjs in TKT-365, so the
// French spec builds its own offer the same way instead of sharing the English one's
// capacity). Every write goes through the back office's real forms.

import { ORGANIZER, signIn, submitForm } from './support.mjs';

export const ADMIN_PATH = '/admin/events/new';
export const VENUE = '00000000-0000-0000-0000-0000000000a1';

async function saveVenueCapacity(page, check, capacity, assertion) {
  const venuePath = `/admin/venues/${VENUE}`;
  await page.goto(venuePath, { waitUntil: 'domcontentloaded' });
  const gaForm = page.locator('form:has(input[value="set-ga"])');
  await gaForm.locator('input[name="ga_capacity"]').fill(String(capacity));
  const capacityRequest = await submitForm(
    page,
    gaForm.getByRole('button', { name: 'Save GA capacity' }),
  );
  const storedCapacity = await gaForm.locator('input[name="ga_capacity"]').inputValue();
  const saved = new URL(capacityRequest.url()).pathname === venuePath && storedCapacity === String(capacity);
  check(assertion, saved, `${capacity}`);
  if (!saved) {
    throw new Error(
      `venue capacity save did not persist ${capacity}: POST ${capacityRequest.url()}, rendered ${storedCapacity}`,
    );
  }
}

/**
 * Signs in, sets the shared venue to `capacity`, creates and publishes one priced GA
 * performance, and restores the venue's capacity. Returns the event and performance ids.
 */
export async function publishOffer(page, check, offer) {
  const { identifier, password, capacity, nameEn, nameFr, startsAt, timezone } = offer;
  await signIn(page, identifier, password);

  const venuePath = `/admin/venues/${VENUE}`;
  await page.goto(venuePath, { waitUntil: 'domcontentloaded' });
  const originalCapacity = Number(
    await page.locator('form:has(input[value="set-ga"]) input[name="ga_capacity"]').inputValue(),
  );
  if (!Number.isSafeInteger(originalCapacity) || originalCapacity < 1) {
    throw new Error(`the shared browser venue has invalid GA capacity ${originalCapacity}`);
  }

  try {
    await saveVenueCapacity(
      page,
      check,
      capacity,
      'the checkout fixture sets its own venue capacity through the browser',
    );

    await page.goto(ADMIN_PATH, { waitUntil: 'domcontentloaded' });

    await page.fill('#name_en', nameEn);
    await page.fill('#name_fr', nameFr);
    await submitForm(page, page.getByRole('button', { name: 'Create event' }));
    const eventId = new URL(page.url()).searchParams.get('event');
    if (!eventId) throw new Error(`event creation did not return an event id: ${page.url()}`);

    await page.selectOption('#venue_id', VENUE);
    await page.fill('#starts_at', startsAt);
    await page.fill('#timezone', timezone);
    await submitForm(page, page.getByRole('button', { name: 'Add the date' }));
    const performanceId = new URL(page.url()).searchParams.get('performance');
    if (!performanceId) throw new Error(`performance creation did not return an id: ${page.url()}`);

    await page.fill('#tt_name_en', 'General admission');
    await page.fill('#tt_name_fr', 'Admission générale');
    await page.fill('#amount', '1250');
    await page.fill('#currency', 'EUR');
    await submitForm(page, page.getByRole('button', { name: 'Set the price' }));

    await submitForm(page, page.getByRole('button', { name: 'Publish' }));
    await page.getByRole('heading', { name: 'Publication accepted' }).waitFor();
    return { eventId, performanceId };
  } finally {
    await saveVenueCapacity(
      page,
      check,
      originalCapacity,
      'the checkout fixture restores the shared venue capacity',
    );
  }
}

export async function readAvailability(page, performanceId) {
  const path = `/api/inventory/slots/${performanceId}/availability?organizer_id=${ORGANIZER}`;
  const response = await page.request.get(path);
  if (!response.ok()) {
    throw new Error(`inventory availability returned ${response.status()}: ${await response.text()}`);
  }
  const body = await response.json();
  for (const field of ['capacity', 'held', 'confirmed', 'available']) {
    if (!Number.isSafeInteger(body[field])) {
      throw new Error(`inventory availability has invalid ${field}: ${JSON.stringify(body)}`);
    }
  }
  return body;
}

export async function waitForAvailability(page, performanceId, expected) {
  const deadline = Date.now() + 20_000;
  let observed;
  while (Date.now() < deadline) {
    try {
      observed = await readAvailability(page, performanceId);
      if (Object.entries(expected).every(([field, value]) => observed[field] === value)) return observed;
    } catch (error) {
      observed = error instanceof Error ? error.message : String(error);
    }
    await page.waitForTimeout(250);
  }
  throw new Error(
    `inventory did not reach ${JSON.stringify(expected)} within 20 seconds; last observed ${JSON.stringify(observed)}`,
  );
}

/**
 * Waits until the published offer is open in inventory with its full capacity AND the
 * event page in `locale` renders its Reserve control, labelled `reserveLabel`.
 */
export async function waitForOffer(page, { eventId, performanceId, capacity, locale, reserveLabel }) {
  const deadline = Date.now() + 20_000;
  let observed;
  while (Date.now() < deadline) {
    try {
      observed = await readAvailability(page, performanceId);
    } catch (error) {
      observed = error instanceof Error ? error.message : String(error);
    }
    const event = await page.goto(`/${locale}/events/${eventId}`, { waitUntil: 'domcontentloaded' });
    if (
      event?.ok() &&
      observed?.capacity === capacity &&
      observed?.held === 0 &&
      observed?.confirmed === 0 &&
      observed?.available === capacity &&
      observed?.offering_status === 'open' &&
      await page.getByRole('button', { name: reserveLabel, exact: true }).count()
    ) {
      return;
    }
    await page.waitForTimeout(250);
  }
  throw new Error(
    `published offer ${eventId} did not become available with capacity ${capacity} within 20 seconds; ` +
    `last observed ${JSON.stringify(observed)}`,
  );
}
