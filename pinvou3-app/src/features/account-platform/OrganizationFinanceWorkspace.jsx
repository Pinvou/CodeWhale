import { PortalDialogScope } from './portal-dialog-scope.jsx';
import React, { useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { OrganizationCreditOverview } from './OrganizationCreditOverview.jsx';
import { accountCenterCopy, organizationCopy, financeCopy, accountLayoutCopy } from '../../shared/i18n.js';
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

import { pointsToMicrocredits, quotaSources, formatRMB, packageAllocatedMicrocredits, productPointFacts, formatAccountDate } from './account-center-view.js';
import { CopyIdentifier } from './CopyIdentifier.jsx';
import { OrganizationOrderHistory } from './OrganizationOrderHistory.jsx';
import { PortalDialog } from './PortalDialog.jsx';

import { useRechargePaymentDialog } from './useRechargePaymentDialog.js';
import { RechargePaymentDialog } from './RechargePaymentDialog.jsx';

export function OrganizationFinanceWorkspace({ client, me, language, presentationTargets, purchaseFocusRequest }) {
 const copy = organizationCopy[language], org = me.active_context.organization_id;
 const center = accountCenterCopy[language];
 const displayDate = value => formatAccountDate(value, language, center.unspecified);
 const api = useMemo(() => new OrganizationClient(client), [client]);
 const sessionEpoch = client.financeSessionEpoch || 0;
 const owner = `${me.user_id}:${org}:${me.active_context.context_version}:${me.active_context.organization_authorization_version}:${sessionEpoch}`;
 const purchaseSection = useRef(null), focusedPurchase = useRef(null);
 const canPurchase = hasPermission(me, 'org.entitlements.purchase');
 const orderClient = useMemo(() => ({
  get financeSessionEpoch() { return client.financeSessionEpoch || 0; },
  rechargeOrder(id, signal) { return api.order(org, id, signal); },
 }), [client, api, org]);
 const [dataReady, setDataReady] = useState(false);
 const [data, setData] = useState({ packages: [], grants: [], products: [] });
 const memberPage = useOrganizationMemberPages({ api, org, owner, enabled: hasPermission(me, 'org.quota.allocate'), quota: true });
 const [order, setOrder] = useState(null), [orderID, setOrderID] = useState(''), [batch, setBatch] = useState(null), [invalidOrder, setInvalidOrder] = useState(false), [historySelection, setHistorySelection] = useState(null), [historyVersion, setHistoryVersion] = useState(0);
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
  setHistorySelection(null); setHistoryVersion(0); setDataReady(false); setData({ packages: [], grants: [], products: [] }); setOrder(null); setOrderID(''); setBatch(null); setError(''); setBusy(true);
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
   setHistoryVersion(value => value + 1);
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
 const query = async (continuePayment = true, requestedID = orderID) => {
  if (busy || !canPurchase || !orderReader.current) return;
  let identifier;
   try { identifier = organizationID(requestedID.trim().toLowerCase()); } catch { setInvalidOrder(true); return; }
   setInvalidOrder(false);
   const stamp = generation.current, ticket = continuePayment ? payment.begin() : payment.current(); setBusy(true); setError('');
  try { const request = orderReader.current.read(identifier); const latest = await acceptOrderRead(await request, request, stamp); if (latest && ownsView(stamp)) { payment.accept(ticket, rememberOrder(latest)); return latest; } }
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
 useEffect(() => {
  if (!purchaseFocusRequest || focusedPurchase.current === purchaseFocusRequest || !dataReady || !canPurchase
    || readDenied.current || purchaseFocusRequest.client !== client || purchaseFocusRequest.actor !== me.user_id
    || purchaseFocusRequest.organizationId !== org || purchaseFocusRequest.contextVersion !== me.active_context.context_version
    || purchaseFocusRequest.sessionEpoch !== sessionEpoch || (client.financeSessionEpoch || 0) !== sessionEpoch) return;
  const focus = () => {
   if (focusedPurchase.current === purchaseFocusRequest || readDenied.current || !currentView.current.canPurchase
     || currentView.current.owner !== owner || (client.financeSessionEpoch || 0) !== sessionEpoch) return;
   if (globalThis.document?.querySelector('[role=dialog], [role=alertdialog]')) return;
   const section = purchaseSection.current;
   if (!section) return;
   focusedPurchase.current = purchaseFocusRequest;
   section.focus(); section.scrollIntoView?.({ block: 'nearest' });
  };
  focus();
  globalThis.document?.addEventListener('focusin', focus);
  return () => globalThis.document?.removeEventListener('focusin', focus);
 }, [purchaseFocusRequest, dataReady, canPurchase, client, me.user_id, me.active_context.context_version, org, sessionEpoch, owner]);
 const purchaseProducts = data.products.filter(product => organizationPaymentChannels(product, data.capabilities).length > 0);
 const sources = quotaSources(data.packages, now);
 const quotaMembers = memberPage.items.filter(member => !member.status || member.status === 'ACTIVE');
 const memberLabel = id => id === me.user_id ? me.username : memberPage.items.find(member => member.user_id === id)?.display_name || `${copy.user} …${id.slice(-6)}`;
 const allocationFields = presentationTargets ? [
  { name: 'user', multiple: true, options: quotaMembers.map(member => ({ value: member.user_id, label: memberLabel(member.user_id) })) },
  { name: 'source', options: sources.map(({ package: item, component }) => ({ value: component.component_id, label: `${localizedProductDisplayName(item, language, 'plan_id')} · ${organizationLabel(copy, 'Component', component.component_type)} · ${center.available} ${formatBalance(component.available_microcredits, language)}` })) },
  { name: 'amount', type: 'text', inputMode: 'decimal', hint: center.amountHint },
 ] : [{ name: 'user' }, { name: 'package' }, { name: 'component' }, { name: 'componentType', options: ['BASE', 'BONUS'].map(value => ({ value, label: organizationLabel(copy, 'Component', value) })) }, { name: 'amount', type: 'text' }];
 const prepareAllocation = values => {
  const users = (presentationTargets ? values.user : values.user.split(',').map(value => value.trim())).map(organizationID);
  if (!users.length || users.length > 200 || new Set(users).size !== users.length) throw new Error('INVALID_ORGANIZATION_INPUT');
  const source = presentationTargets ? sources.find(item => item.component.component_id === values.source) : null;
  const amount = presentationTargets ? integerMoney(pointsToMicrocredits(values.amount)) : integerMoney(values.amount);
  const type = presentationTargets ? source?.component.component_type : values.componentType;
  if (amount <= 0n || !['BASE', 'BONUS'].includes(type) || presentationTargets && (!source || amount * BigInt(users.length) > integerMoney(source.component.available_microcredits)) || !presentationTargets && amount.toString() !== values.amount) throw new Error('INVALID_ORGANIZATION_INPUT');
  return { items: users.map(user => { const member = quotaMembers.find(item => item.user_id === user); return { member_user_id: user, membership_version: organizationVersion(member?.version), package_id: organizationID(presentationTargets ? source.package.package_id : values.package), component_id: organizationID(presentationTargets ? source.component.component_id : values.component), component_type: type, amount_microcredits: amount.toString() }; }) };
 };
 const ended = item => ['EXPIRED', 'REVOKED', 'CONSUMED'].includes(item.status) || Boolean(item.valid_until || item.expires_at) && Date.parse(item.valid_until || item.expires_at) <= now;
 const packageCard = item => <article className="ap-benefit-card" key={item.package_id}><header><strong>{localizedProductDisplayName(item, language, 'plan_id')}</strong><span className="ap-status" data-status={item.status}>{organizationLabel(copy, 'Status', item.status)}</span></header><dl><dt>{center.total}</dt><dd>{formatBalance(item.points_total_microcredits, language)}</dd><dt>{center.available}</dt><dd>{formatBalance(item.points_remaining_microcredits, language)}</dd><dt>{center.reserved}</dt><dd>{formatBalance(item.points_reserved_microcredits, language)}</dd><dt>{center.allocated}</dt><dd>{packageAllocatedMicrocredits(item) === null ? center.unspecified : formatBalance(packageAllocatedMicrocredits(item), language)}</dd><dt>{center.expires}</dt><dd>{displayDate(item.valid_until || item.expires_at)}</dd></dl>{(Array.isArray(item.components) ? item.components : []).map(component => <p key={component.component_id || component.component_type}>{organizationLabel(copy, 'Component', component.component_type)} · {center.available}: {formatBalance(component.available_microcredits, language)}{component.status && <span className="ap-status" data-status={component.status}>{organizationLabel(copy, 'Status', component.status)}</span>}</p>)}<details><summary>{center.technicalDetails}</summary><CopyIdentifier value={item.package_id} language={language} /></details></article>;
 const grantCard = grant => <div key={grant.grant_id}><header><strong>{memberLabel(grant.member_user_id)}</strong><span className="ap-status" data-status={grant.status}>{organizationLabel(copy, 'Status', grant.status)}</span></header><dl className="ap-quota-facts"><dt>{center.total}</dt><dd>{formatBalance(grant.original_microcredits, language)}</dd><dt>{center.available}</dt><dd>{formatBalance(grant.available_microcredits, language)}</dd><dt>{center.reserved}</dt><dd>{formatBalance(grant.reserved_microcredits, language)}</dd><dt>{center.used}</dt><dd>{formatBalance(grant.used_microcredits, language)}</dd><dt>{center.expires}</dt><dd>{displayDate(grant.expires_at)}</dd></dl><details><summary>{center.technicalDetails}</summary><CopyIdentifier value={grant.grant_id} language={language} /></details>{can('org.quota.revoke') && grant.status === 'ACTIVE' && <OrganizationActionForm client={client} owner={owner} copy={{ ...copy, confirm: center.quotaRevoke }} scope={`quota-revoke:${grant.grant_id}`} confirmationTarget={`${org} · ${grant.member_user_id} · ${grant.grant_id}`} title={copy.revoke} tone="danger" fields={[{ name: 'reason' }]} prepare={values => { if (!values.reason.trim()) throw new Error('INVALID_ORGANIZATION_INPUT'); return { id: grant.grant_id, reason: values.reason.trim() }; }} action={(facts, key) => api.revokeGrant(org, facts.id, facts.reason, key)} onConfirmed={() => void refresh()} confirm />}</div>;
 const workspace = <section className={presentationTargets ? 'ap-org-finance' : undefined} tabIndex={-1}>{historySelection?.owner === owner && order?.order_id === historySelection.id && <PortalDialog title={center.orderDetails} closeLabel={accountLayoutCopy[language].closeDialog} busy={busy} onClose={() => setHistorySelection(null)}><dl><dt>{copy.order}</dt><dd><CopyIdentifier value={order.order_id} language={language} /></dd><dt>{copy.amount}</dt><dd>{formatRMB(order.amount_fen, language)}</dd><dt>{copy.status}</dt><dd>{organizationLabel(copy, 'Status', order.status)}</dd><dt>{center.createdAt}</dt><dd>{displayDate(historySelection.createdAt)}</dd><dt>{center.paidAt}</dt><dd>{displayDate(order.paid_at)}</dd><dt>{center.expires}</dt><dd>{displayDate(order.expires_at)}</dd></dl>{isPayablePaymentOrder(order, now) && <div className="ap-dialog-actions"><button type="button" className="ap-primary" onClick={() => { setHistorySelection(null); void query(true, order.order_id); }}>{financeCopy[language].continuePayment}</button></div>}</PortalDialog>}{payment.order && <RechargePaymentDialog error={error} order={payment.order} copy={financeCopy[language]} language={language} now={now} busy={busy} onDismiss={payment.dismiss} onQuery={() => void query(false)} canRestoreFocus={payment.canRestoreFocus} returnFocus={payment.trigger} />}<div className="ap-section-heading"><h3>{center.billing}</h3><button disabled={busy} onClick={() => void refresh()}>{presentationTargets ? center.refreshCredits : copy.refresh}</button></div>{!presentationTargets && error && <p role="alert">{error}</p>}{!presentationTargets && busy && <p role="status">{copy.loading}</p>}
  {can('org.wallet.read') && data.balance && <dl><dt>{copy.available}</dt><dd>{formatBalance(data.balance.available_microcredits, language)}</dd><dt>{copy.reserved}</dt><dd>{formatBalance(data.balance.reserved_microcredits, language)}</dd></dl>}
  {can('org.wallet.read') && <section><h4>{copy.packages}</h4>{!busy && !error && !data.packages.length && <p className="ap-empty">{center.noPackages}</p>}<div className="ap-benefit-grid">{data.packages.filter(item => !ended(item)).map(packageCard)}{data.packages.some(ended) && <details className="ap-history-group"><summary>{center.historyBenefits}</summary>{data.packages.filter(ended).map(packageCard)}</details>}</div></section>}
  {can('org.entitlements.purchase') && <section ref={purchaseSection} tabIndex={-1} data-organization-purchase={org}>{data.capabilities && (!presentationTargets || data.capabilities.test_only || !data.capabilities.formal_collection_admitted) && <p role="status">{data.capabilities.test_only ? copy.testCollection : data.capabilities.formal_collection_admitted ? copy.formalCollection : copy.collectionUnavailable}</p>}{!busy && !purchaseProducts.length && <p>{copy.noPurchaseChannel}</p>}<OrganizationActionForm requireSelections compactUnavailable disabled={!purchaseProducts.length} client={client} owner={owner} copy={copy} scope="organization-purchase" title={copy.purchase} tone="primary" disabledReason={busy ? copy.loading : data.products.length ? copy.noPurchaseChannel : center.noProducts} fields={[{ name: 'product', hint: values => { const product = purchaseProducts.find(item => item.product_id === values.product); const facts = product && productPointFacts(product); return product ? center.credited + ': ' + (facts ? facts.total.toLocaleString() : center.unspecified) + ' · ' + center.validity + ': ' + (Number.isSafeInteger(product.validity_days) && product.validity_days > 0 ? center.days(product.validity_days) : center.unspecified) : center.chooseProduct; }, options: purchaseProducts.map(product => ({ value: product.product_id, label: `${localizedProductDisplayName(product, language, 'product_id')} · ${formatRMB(product.amount_fen, language)}` })) }, { name: 'channel', emptyHint: values => values.product ? copy.noPurchaseChannel : center.chooseProduct, options: values => organizationPaymentChannels(purchaseProducts.find(product => product.product_id === values.product), data.capabilities).map(value => ({ value, label: organizationLabel(copy, 'Channel', value) })) }]} prepare={values => { const product = purchaseProducts.find(item => item.product_id === values.product); const channel = effectivePaymentChannel(organizationPaymentChannels(product, data.capabilities), values.channel); if (!product || !channel || channel !== values.channel) throw new Error('INVALID_ORGANIZATION_INPUT'); return { product_id: product.product_id, channel }; }} action={(facts, key) => { purchasePresentation.current = payment.begin(); return api.createOrder(org, facts, key); }} onConfirmed={result => { if (ownsView(generation.current) && currentView.current.canPurchase) { const latest = rememberOrder(result); setOrder(paymentOrderFacts(latest)); setOrderID(latest.order_id); setHistoryVersion(value => value + 1); payment.accept(purchasePresentation.current, latest); } }} />
   <form className="ap-order-lookup" onSubmit={event => { event.preventDefault(); void query(); }}><label>{center.orderLookup}<input value={orderID} aria-invalid={invalidOrder} onChange={event => { setInvalidOrder(false); setOrderID(event.target.value); }} /></label><button disabled={busy || !orderID.trim()}>{copy.queryOrder}</button></form>{invalidOrder && <p className="ap-field-hint error" role="alert">{center.invalidOrder}</p>}
   {order && <div><p>{order.order_id} · {organizationLabel(copy, 'Status', order.status)} · {organizationLabel(copy, 'Status', order.fulfillment_status)} · {formatFen(order.amount_fen, language)}</p></div>}
  </section>}
  {presentationTargets && canPurchase && <OrganizationOrderHistory api={api} org={org} owner={owner} language={language} version={historyVersion} enabled={!readDenied.current} busy={busy} onSelect={(id, row) => { const stamp = generation.current; setHistorySelection(null); setOrderID(id); void query(false, id).then(latest => { if (latest && ownsView(stamp)) setHistorySelection({ id, owner, createdAt: row.created_at }); }); }} />}
   {can('org.quota.read.self') && <section><h4>{copy.quota}</h4>{!busy && !error && !data.grants.length && <p className="ap-empty">{center.noQuota}</p>}{data.grants.filter(item => !ended(item)).map(grantCard)}{data.grants.some(ended) && <details className="ap-history-group"><summary>{center.historyQuotas}</summary>{data.grants.filter(ended).map(grantCard)}</details>}</section>}
  {can('org.quota.allocate') && <section><p>{copy.membersPageHint}</p>{memberPage.loading && <p role="status">{copy.loading}</p>}{memberPage.error && <p role="alert">{copy[memberPage.error]}</p>}<div className="ap-action-group"><button type="button" disabled={memberPage.loading} onClick={() => void memberPage.refresh()}>{copy.refresh}</button><button type="button" disabled={memberPage.loading || !memberPage.nextCursor} onClick={() => void memberPage.loadMore()}>{copy.loadMoreMembers}</button></div><p className="ap-muted">{presentationTargets ? center.allocationHint : copy.membersPageHint}</p>{!presentationTargets && <ul>{memberPage.items.map(member => <li key={member.user_id}>{member.display_name || member.user_id} / {member.user_id} / {member.version}</li>)}</ul>}<OrganizationActionForm client={client} owner={owner} copy={presentationTargets ? { ...copy, user: center.allocatedMember, source: center.entitlement, amount: center.allocationAmount } : copy} scope="quota-allocation" compactUnavailable title={copy.allocate} tone="primary" confirm={Boolean(presentationTargets)} confirmationTarget={presentationTargets?.organizationName || copy.title} disabled={Boolean(presentationTargets) && (!sources.length || !quotaMembers.length)} disabledReason={busy || memberPage.loading ? copy.loading : !quotaMembers.length ? center.noTargets : center.noSources} fields={allocationFields} prepare={prepareAllocation} action={(facts, key) => api.allocate(org, facts, key)} onConfirmed={result => { setBatch(result); void refresh(); }} />
   {batch && <div>{batch.items.some(item => item.status === 'FAILED') && <p role="alert">{copy.partial}</p>}<ul>{batch.items.map(item => <li key={item.index}>{item.index + 1} · {organizationLabel(copy, 'Status', item.status)} · {item.grant?.grant_id || (organizationLabel(copy, 'Code', item.code) === item.code ? item.code : `${organizationLabel(copy, 'Code', item.code)} (${item.code})`)}</li>)}</ul></div>}
  </section>}
 </section>;
 if (!presentationTargets) return workspace;
 return <>
  {presentationTargets.credits && createPortal(<PortalDialogScope active={presentationTargets.section === 'creditsUsage'}><OrganizationCreditOverview me={me} data={data} language={language} ready={dataReady} error={error} now={now} onPurchase={presentationTargets.onPurchase} /></PortalDialogScope>, presentationTargets.credits)}
  {presentationTargets.management && createPortal(<PortalDialogScope active={presentationTargets.section === 'myOrganization' && (!presentationTargets.organizationSection || presentationTargets.organizationSection === 'billing')}>{workspace}</PortalDialogScope>, presentationTargets.management)}
  {presentationTargets.feedback && createPortal(<>{error && <p className="ap-alert error" role="alert">{error}</p>}{busy && <p role="status">{copy.loading}</p>}</>, presentationTargets.feedback)}
 </>;
}
