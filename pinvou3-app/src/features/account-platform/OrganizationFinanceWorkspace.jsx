import { PortalDialogScope } from './portal-dialog-scope.jsx';
import React, { useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { OrganizationCreditOverview } from './OrganizationCreditOverview.jsx';
import { organizationCopy, financeCopy } from '../../shared/i18n.js';
import { organizationLabel } from './organization-labels.js';
import { OrganizationClient, organizationID, organizationVersion, organizationPaymentChannels } from './organization-client.js';
import { useOrganizationMemberPages } from './useOrganizationMemberPages.js';
import { OrganizationActionForm } from './OrganizationActionForm.jsx';
import { hasPermission } from './authorization-ui.js';
import { formatBalance } from './balance-view.js';
import { integerMoney, formatFen } from './finance-contract.js';
import { isPayablePaymentOrder, effectivePaymentChannel, paymentOrderFacts } from './order-view.js';
import { localizedProductDisplayName } from './localized-product-display-name.js';
import { createRechargeOrderReader, refreshPaidRechargeRead, startRechargeOrderPolling } from './recharge-order-polling.js';

import { useRechargePaymentDialog } from './useRechargePaymentDialog.js';
import { RechargePaymentDialog } from './RechargePaymentDialog.jsx';

export function OrganizationFinanceWorkspace({ client, me, language, presentationTargets }) {
 const copy = organizationCopy[language], org = me.active_context.organization_id;
 const api = useMemo(() => new OrganizationClient(client), [client]);
 const sessionEpoch = client.financeSessionEpoch || 0;
 const owner = `${me.user_id}:${org}:${me.active_context.context_version}:${me.active_context.organization_authorization_version}:${sessionEpoch}`;
 const canPurchase = hasPermission(me, 'org.entitlements.purchase');
 const orderClient = useMemo(() => ({
  get financeSessionEpoch() { return client.financeSessionEpoch || 0; },
  rechargeOrder(id, signal) { return api.order(org, id, signal); },
 }), [client, api, org]);
 const [dataReady, setDataReady] = useState(false);
 const [data, setData] = useState({ packages: [], grants: [], products: [] });
 const memberPage = useOrganizationMemberPages({ api, org, owner, enabled: hasPermission(me, 'org.quota.allocate'), quota: true });
 const [order, setOrder] = useState(null), [orderID, setOrderID] = useState(''), [batch, setBatch] = useState(null);
 const [error, setError] = useState(''), [busy, setBusy] = useState(false), [now, setNow] = useState(Date.now());
 const readDenied = useRef(false), purchasePresentation = useRef(null), withdrawnOrders = useRef(new Map());
 const generation = useRef(0), can = permission => hasPermission(me, permission);
 const mounted = useRef(false), orderReader = useRef(null), paidReads = useRef(new WeakSet()), currentView = useRef(null);
 currentView.current = { client, api, owner, org, sessionEpoch, canPurchase, order, busy };
 const ownsView = stamp => {
  const current = currentView.current;
  return mounted.current && stamp === generation.current && current.client === client && current.api === api
   && current.owner === owner && current.org === org && current.sessionEpoch === sessionEpoch
   && (client.financeSessionEpoch || 0) === sessionEpoch && !readDenied.current;
 };
 const payment = useRechargePaymentDialog({ client, owner, sessionEpoch, enabled: canPurchase && !readDenied.current, now,
  isCurrent: () => ownsView(generation.current) });
 // Only this authorized view owns these sanitized facts. A pending response
 // generated before a confirmed nonpayable result cannot restore instructions.
 const rememberOrder = result => {
  const latest = payment.boundOrder(result, true), previous = withdrawnOrders.current.get(result.order_id);
  if (previous && ['CREATED', 'PENDING'].includes(latest.status)) return { ...latest, ...previous };
  if (!['CREATED', 'PENDING'].includes(latest.status) || latest.close_status !== 'OPEN') {
   withdrawnOrders.current.set(latest.order_id, paymentOrderFacts(latest));
  }
  return latest;
 };
 const readFailure = reason => {
  if (reason.status === 401 || reason.status === 403) { readDenied.current = true; payment.dismiss(); setOrder(null); }
  setError(reason.status === 403 ? copy.permissions : copy.unavailable);
 };
 const resetReader = stamp => {
  orderReader.current?.dispose();
  orderReader.current = canPurchase ? createRechargeOrderReader(orderClient, () => ownsView(stamp) && currentView.current.canPurchase) : null;
 };

 const purchaseRead = async (request, stamp) => {
  try { return await request; } catch (reason) { if (ownsView(stamp)) readFailure(reason); throw reason; }
 };
 const load = async (stamp, signal) => {
  const [balance, packages, grants, products, capabilities] = await Promise.all([
   can('org.wallet.read') ? api.balance(org, signal) : null,
   can('org.wallet.read') ? api.packages(org, signal) : [],
   can('org.quota.read.self') ? api.grants(org, signal) : [],
   can('org.entitlements.purchase') ? purchaseRead(api.products(org, signal), stamp) : [],
   can('org.entitlements.purchase') ? purchaseRead(api.capabilities(org, signal), stamp) : null,
  ]);
  if (ownsView(stamp)) { setData({ balance, packages, grants, products, capabilities }); setDataReady(true); }
 };
 const refresh = async () => { void memberPage.refresh(); const stamp = ++generation.current; resetReader(stamp); payment.dismiss(); setBusy(true); setError(''); try { await load(stamp); } catch (reason) { if (ownsView(stamp)) setError(reason.status === 403 ? copy.permissions : copy.unavailable); } finally { if (ownsView(stamp)) setBusy(false); } };
 useEffect(() => {
  const stamp = ++generation.current, controller = new AbortController(); mounted.current = true; readDenied.current = false; withdrawnOrders.current.clear(); payment.dismiss(); resetReader(stamp);
  setDataReady(false); setData({ packages: [], grants: [], products: [] }); setOrder(null); setOrderID(''); setBatch(null); setError(''); setBusy(true);
  void load(stamp, controller.signal).catch(reason => { if (ownsView(stamp) && reason.name !== 'AbortError') setError(reason.status === 403 ? copy.permissions : copy.unavailable); }).finally(() => { if (ownsView(stamp)) setBusy(false); });
  return () => { mounted.current = false; generation.current++; withdrawnOrders.current.clear(); orderReader.current?.dispose(); orderReader.current = null; controller.abort(); };
 }, [api, owner, sessionEpoch, canPurchase]); // eslint-disable-line react-hooks/exhaustive-deps
 useEffect(() => { const interval = globalThis.setInterval(() => setNow(Date.now()), 1000); return () => globalThis.clearInterval(interval); }, []);
 const acceptOrderRead = async (latest, request, stamp) => {
  if (!ownsView(stamp) || !currentView.current.canPurchase) return;
  latest = rememberOrder(latest);
  setOrder(paymentOrderFacts(latest));
  const ticket = payment.current(); if (ticket?.order?.order_id === latest.order_id) payment.accept(ticket, latest);
  await refreshPaidRechargeRead(latest, request, paidReads.current, async () => {
   try { await load(stamp); }
   catch (reason) {
    // Product/capability denials retire their own purchase scope in purchaseRead.
    // A separate wallet refresh failure must remain visible without revoking it.
    if (ownsView(stamp) && reason.name !== 'AbortError') {
     if (reason.status === 401) readFailure(reason);
     else setError(reason.status === 403 ? copy.permissions : copy.unavailable);
    }
   }
  });
  return latest;
 };
 const query = async (continuePayment = true) => {
  if (busy || !canPurchase || !orderReader.current) return;
  const stamp = generation.current, ticket = continuePayment ? payment.begin() : payment.current(); setBusy(true); setError('');
  try { const request = orderReader.current.read(organizationID(orderID)); const latest = await acceptOrderRead(await request, request, stamp); if (latest && ownsView(stamp)) payment.accept(ticket, rememberOrder(latest)); }
  catch (reason) { if (ownsView(stamp) && reason.name !== 'AbortError') readFailure(reason); }
  finally { if (ownsView(stamp)) setBusy(false); }
 };
 useEffect(() => {
  if (!canPurchase || busy || !isPayablePaymentOrder(payment.order, Date.now()) || !orderReader.current) return undefined;
  const stamp = generation.current, reader = orderReader.current, selectedOrderID = payment.order.order_id;
  const isCurrent = () => ownsView(stamp) && orderReader.current === reader && currentView.current.canPurchase
   && !currentView.current.busy && payment.current()?.order?.order_id === selectedOrderID;
  const ownsCompletedRead = () => ownsView(stamp) && orderReader.current === reader && currentView.current.canPurchase
   && !currentView.current.busy && currentView.current.order?.order_id === selectedOrderID;
  return startRechargeOrderPolling({ reader, getOrder: () => payment.current()?.order, isCurrent,
   allowLatePaid: ownsCompletedRead,
   accept: async (latest, request) => { if (isCurrent() || ownsCompletedRead()) await acceptOrderRead(latest, request, stamp); },
   onError: reason => { if (isCurrent()) readFailure(reason); },
  });
 }, [orderClient, owner, sessionEpoch, canPurchase, busy, payment.order?.order_id, payment.order?.status, payment.order?.close_status, payment.order?.expires_at]); // eslint-disable-line react-hooks/exhaustive-deps
 const purchaseProducts = data.products.filter(product => organizationPaymentChannels(product, data.capabilities).length > 0);
 const workspace = <section tabIndex={-1}>{payment.order && <RechargePaymentDialog error={error} order={payment.order} copy={financeCopy[language]} language={language} now={now} busy={busy} onDismiss={payment.dismiss} onQuery={() => void query(false)} canRestoreFocus={payment.canRestoreFocus} returnFocus={payment.trigger} />}<h3>{copy.balance}</h3><button disabled={busy} onClick={() => void refresh()}>{copy.refresh}</button>{!presentationTargets && error && <p role="alert">{error}</p>}{!presentationTargets && busy && <p role="status">{copy.loading}</p>}
  {can('org.wallet.read') && data.balance && <dl><dt>{copy.available}</dt><dd>{formatBalance(data.balance.available_microcredits, language)}</dd><dt>{copy.reserved}</dt><dd>{formatBalance(data.balance.reserved_microcredits, language)}</dd></dl>}
  {can('org.wallet.read') && <section><h4>{copy.packages}</h4>{data.packages.map(item => <div key={item.package_id}><p>{item.plan_name} · {item.package_id} · {organizationLabel(copy, 'Status', item.status)} · {formatBalance(item.points_remaining_microcredits, language)}</p><ul>{item.components?.map(component => <li key={component.component_id}>{component.component_id} · {organizationLabel(copy, 'Component', component.component_type)} · {organizationLabel(copy, 'Status', component.status)} · {formatBalance(component.available_microcredits, language)}</li>)}</ul></div>)}</section>}
  {can('org.entitlements.purchase') && <section>{data.capabilities && <p role="status">{data.capabilities.test_only ? copy.testCollection : data.capabilities.formal_collection_admitted ? copy.formalCollection : copy.collectionUnavailable}</p>}{!busy && !purchaseProducts.length && <p>{copy.noPurchaseChannel}</p>}<OrganizationActionForm disabled={!purchaseProducts.length} client={client} owner={owner} copy={copy} scope="organization-purchase" title={copy.purchase} fields={[{ name: 'product', options: purchaseProducts.map(product => ({ value: product.product_id, label: `${localizedProductDisplayName(product, language)} · ${formatFen(product.amount_fen, language)}` })) }, { name: 'channel', options: values => organizationPaymentChannels(purchaseProducts.find(product => product.product_id === values.product), data.capabilities).map(value => ({ value, label: organizationLabel(copy, 'Channel', value) })) }]} prepare={values => { const product = purchaseProducts.find(item => item.product_id === values.product); const channel = effectivePaymentChannel(organizationPaymentChannels(product, data.capabilities), values.channel); if (!product || !channel || channel !== values.channel) throw new Error('INVALID_ORGANIZATION_INPUT'); return { product_id: product.product_id, channel }; }} action={(facts, key) => { purchasePresentation.current = payment.begin(); return api.createOrder(org, facts, key); }} onConfirmed={result => { if (ownsView(generation.current) && currentView.current.canPurchase) { const latest = rememberOrder(result); setOrder(paymentOrderFacts(latest)); setOrderID(latest.order_id); payment.accept(purchasePresentation.current, latest); } }} />
   <form onSubmit={event => { event.preventDefault(); void query(); }}><label>{copy.order}<input value={orderID} onChange={event => setOrderID(event.target.value)} /></label><button disabled={busy || !orderID}>{copy.queryOrder}</button></form>
   {order && <div><p>{order.order_id} · {organizationLabel(copy, 'Status', order.status)} · {organizationLabel(copy, 'Status', order.fulfillment_status)} · {formatFen(order.amount_fen, language)}</p></div>}
  </section>}
  {can('org.quota.read.self') && <section><h4>{copy.quota}</h4>{data.grants.map(grant => <div key={grant.grant_id}><p>{grant.member_user_id} · {grant.grant_id} · {organizationLabel(copy, 'Status', grant.status)} · {formatBalance(grant.available_microcredits, language)} · {formatBalance(grant.reserved_microcredits, language)}</p>{can('org.quota.revoke') && grant.status === 'ACTIVE' && <OrganizationActionForm client={client} owner={owner} copy={copy} scope={`quota-revoke:${grant.grant_id}`} confirmationTarget={`${org} · ${grant.member_user_id} · ${grant.grant_id}`} title={copy.revoke} fields={[{ name: 'reason' }]} prepare={values => { if (!values.reason.trim()) throw new Error('INVALID_ORGANIZATION_INPUT'); return { id: grant.grant_id, reason: values.reason.trim() }; }} action={(facts, key) => api.revokeGrant(org, facts.id, facts.reason, key)} onConfirmed={() => void refresh()} confirm />}</div>)}</section>}
  {can('org.quota.allocate') && <section><p>{copy.membersPageHint}</p>{memberPage.loading && <p role="status">{copy.loading}</p>}{memberPage.error && <p role="alert">{copy[memberPage.error]}</p>}<button type="button" disabled={memberPage.loading} onClick={() => void memberPage.refresh()}>{copy.refresh}</button><button type="button" disabled={memberPage.loading || !memberPage.nextCursor} onClick={() => void memberPage.loadMore()}>{copy.loadMoreMembers}</button><ul>{memberPage.items.map(member => <li key={member.user_id}>{member.display_name || member.user_id} · {member.user_id} · {member.version}</li>)}</ul><OrganizationActionForm client={client} owner={owner} copy={copy} scope="quota-allocation" title={copy.allocate} fields={[{ name: 'user' }, { name: 'package' }, { name: 'component' }, { name: 'componentType', options: ['BASE', 'BONUS'].map(value => ({ value, label: organizationLabel(copy, 'Component', value) })) }, { name: 'amount', type: 'text' }]} prepare={values => { const users = values.user.split(',').map(value => organizationID(value.trim())); if (!users.length || users.length > 200 || new Set(users).size !== users.length) throw new Error('INVALID_ORGANIZATION_INPUT'); const amount = integerMoney(values.amount); if (amount <= 0n || amount.toString() !== values.amount || !['BASE', 'BONUS'].includes(values.componentType)) throw new Error('INVALID_ORGANIZATION_INPUT'); return { items: users.map(user => { const member = memberPage.items.find(item => item.user_id === user); return { member_user_id: user, membership_version: organizationVersion(member?.version), package_id: organizationID(values.package), component_id: organizationID(values.component), component_type: values.componentType, amount_microcredits: amount.toString() }; }) }; }} action={(facts, key) => api.allocate(org, facts, key)} onConfirmed={result => { setBatch(result); void refresh(); }} />
   {batch && <div>{batch.items.some(item => item.status === 'FAILED') && <p role="alert">{copy.partial}</p>}<ul>{batch.items.map(item => <li key={item.index}>{item.index + 1} · {organizationLabel(copy, 'Status', item.status)} · {item.grant?.grant_id || (organizationLabel(copy, 'Code', item.code) === item.code ? item.code : `${organizationLabel(copy, 'Code', item.code)} (${item.code})`)}</li>)}</ul></div>}
  </section>}
 </section>;
 if (!presentationTargets) return workspace;
 return <>
  {presentationTargets.credits && createPortal(<PortalDialogScope active={presentationTargets.section === 'creditsUsage'}><OrganizationCreditOverview me={me} data={data} language={language} ready={dataReady} error={error} now={now} /></PortalDialogScope>, presentationTargets.credits)}
  {presentationTargets.management && createPortal(<PortalDialogScope active={presentationTargets.section === 'myOrganization'}>{workspace}</PortalDialogScope>, presentationTargets.management)}
  {presentationTargets.feedback && createPortal(<>{error && <p className="ap-alert error" role="alert">{error}</p>}{busy && <p role="status">{copy.loading}</p>}</>, presentationTargets.feedback)}
 </>;
}
