import { reconcilePath } from './scanner-controls.mjs';
// Runner-only transport mutations. The database assertions stay unchanged.
export async function installTransportMutation(context) {
  const mutation = process.env.SCANNER_MUTATION;
  if (!['AX01','AX02'].includes(mutation)) return;
  await context.route(reconcilePath, route => {
    const body = route.request().postDataJSON();
    for (const row of body.occurrences) {
      if (mutation === 'AX01' && row.event_type === 'entry') row.event_type = 'exit';
      if (mutation === 'AX02') delete row.local_decision;
    }
    return route.continue({ postData: JSON.stringify(body) });
  });
}
