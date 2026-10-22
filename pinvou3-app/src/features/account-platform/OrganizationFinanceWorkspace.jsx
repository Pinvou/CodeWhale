import React, { useEffect, useMemo, useRef, useState } from 'react';
import { organizationCopy } from '../../shared/i18n.js';
import { organizationLabel } from './organization-labels.js';
import { OrganizationClient, organizationID, organizationVersion, organizationPaymentChannels } from './organization-client.js';
import { useOrganizationMemberPages } from './useOrganizationMemberPages.js';
import { OrganizationActionForm } from './OrganizationActionForm.jsx';
import { hasPermission } from './authorization-ui.js';
import { formatBalance } from './balance-view.js';
import { integerMoney, formatFen, safePaymentURL, safePaymentQR } from './finance-contract.js';
import { isPayablePaymentOrder, effectivePaymentChannel } from './order-view.js';
import { localizedProductDisplayName } from './localized-product-display-name.js';
import { createRechargeOrderReader, refreshPaidRechargeRead, startRechargeOrderPolling } from './recharge-order-polling.js';

export function OrganizationFinanceWorkspace({ client, me, language }) {
 const copy = organizationCopy[language], org = me.active_context.organization_id;
 const api = useMemo(() => new OrganizationClient(client), [client]);
 const sessionEpoch = client.financeSessionEpoch || 0;
 const owner = `${me.user_id}:${org}:${me.active_context.context_version}:${me.active_context.organization_authorization_version}:${sessionEpoch}`;
 const canPurchase = hasPermission(me, 'org.entitlements.purchase');
 const orderClient = useMemo(() => ({
  get financeSessionEpoch() { return client.financeSessionEpoch || 0; },
  rechargeOrder(id, signal) { return api.order(org, id, signal); },
 }), [client, api, org]);
 const [data, setData] = useState({ packages: [], grants: [], products: [] });
 const memberPage = useOrganizationMemberPages({ api, org, owner, enabled: hasPermission(me, 'org.quota.allocate'), quota: true });
 const [order, setOrder] = useState(null), [orderID, setOrderID] = useState(''), [batch, setBatch] = useState(null);
 const [error, setError] = useState(''), [busy, setBusy] = useState(false), [now, setNow] = useState(Date.now());
 const generation = useRef(0), can = permission => hasPermission(me, permission);
 const mounted = useRef(false), orderReader = useRef(null), paidReads = useRef(new WeakSet()), currentView = useRef(null);
 currentView.current = { client, api, owner, org, sessionEpoch, canPurchase, order, busy };
 const ownsView = stamp => {
  const current = currentView.current;
  return mounted.current && stamp === generation.current && current.client === client && current.api === api
   && current.owner === owner && current.org === org && current.sessionEpoch === sessionEpoch
   && (client.financeSessionEpoch || 0) === sessionEpoch;
 };
 const resetReader = stamp => {
  orderReader.current?.dispose();
  orderReader.current = canPurchase ? createRechargeOrderReader(orderClient, () => ownsView(stamp) && currentView.current.canPurchase) : null;
 };

 const load = async (stamp, signal) => {
  const [balance, packages, grants, products, capabilities] = await Promise.all([
   can('org.wallet.read') ? api.balance(org, signal) : null,
   can('org.wallet.read') ? api.packages(org, signal) : [],
   can('org.quota.read.self') ? api.grants(org, signal) : [],
   can('org.entitlements.purchase') ? api.products(org, signal) : [],
   can('org.entitlements.purchase') ? api.capabilities(org, signal) : null,
  ]);
  if (ownsView(stamp)) setData({ balance, packages, grants, products, capabilities });
 };
 const refresh = async () => { void memberPage.refresh(); const stamp = ++generation.current; resetReader(stamp); setBusy(true); setError(''); try { await load(stamp); } catch (reason) { if (ownsView(stamp)) setError(reason.status === 403 ? copy.permissions : copy.unavailable); } finally { if (ownsView(stamp)) setBusy(false); } };
 useEffect(() => {
  const stamp = ++generation.current, controller = new AbortController(); mounted.current = true; resetReader(stamp);
  setData({ packages: [], grants: [], products: [] }); setOrder(null); setOrderID(''); setBatch(null); setError(''); setBusy(true);
  void load(stamp, controller.signal).catch(reason => { if (ownsView(stamp) && reason.name !== 'AbortError') setError(reason.status === 403 ? copy.permissions : copy.unavailable); }).finally(() => { if (ownsView(stamp)) setBusy(false); });
  return () => { mounted.current = false; generation.current++; orderReader.current?.dispose(); orderReader.current = null; controller.abort(); };
 }, [api, owner, sessionEpoch, canPurchase]); // eslint-disable-line react-hooks/exhaustive-deps
 useEffect(() => { const interval = globalThis.setInterval(() => setNow(Date.now()), 1000); return () => globalThis.clearInterval(interval); }, []);
 const acceptOrderRead = async (latest, request, stamp) => {
  if (!ownsView(stamp) || !currentView.current.canPurchase) return;
  setOrder(latest);
  await refreshPaidRechargeRead(latest, request, paidReads.current, () => load(stamp));
 };
 const query = async () => {
  if (busy || !canPurchase || !orderReader.current) return;
  const stamp = generation.current; setBusy(true); setError('');
  try { const request = orderReader.current.read(organizationID(orderID)); await acceptOrderRead(await request, request, stamp); }
  catch (reason) { if (ownsView(stamp) && reason.name !== 'AbortError') setError(reason.status === 403 ? copy.permissions : copy.unavailable); }
  finally { if (ownsView(stamp)) setBusy(false); }
 };
 useEffect(() => {
  if (!canPurchase || busy || !isPayablePaymentOrder(order, Date.now()) || !orderReader.current) return undefined;
  const stamp = generation.current, reader = orderReader.current, selectedOrderID = order.order_id;
  const isCurrent = () => ownsView(stamp) && orderReader.current === reader && currentView.current.canPurchase
   && !currentView.current.busy && currentView.current.order?.order_id === selectedOrderID;
  return startRechargeOrderPolling({ reader, getOrder: () => currentView.current.order, isCurrent,
   accept: async (latest, request) => { if (isCurrent()) await acceptOrderRead(latest, request, stamp); },
   onError: reason => { if (isCurrent()) setError(reason.status === 403 ? copy.permissions : copy.unavailable); },
  });
 }, [orderClient, owner, sessionEpoch, canPurchase, busy, order?.order_id, order?.status, order?.close_status, order?.expires_at]); // eslint-disable-line react-hooks/exhaustive-deps
 const purchaseProducts = data.products.filter(product => organizationPaymentChannels(product, data.capabilities).length > 0);
 const paymentURL = isPayablePaymentOrder(order, now) ? safePaymentURL(order?.payment_url) : '';
 const paymentQR = isPayablePaymentOrder(order, now) ? safePaymentQR(order?.qr_data) : '';
 return <section><h3>{copy.balance}</h3><button disabled={busy} onClick={() => void refresh()}>{copy.refresh}</button>{error && <p role="alert">{error}</p>}{busy && <p role="status">{copy.loading}</p>}
  {data.balance && <dl><dt>{copy.available}</dt><dd>{formatBalance(data.balance.available_microcredits, language)}</dd><dt>{copy.reserved}</dt><dd>{formatBalance(data.balance.reserved_microcredits, language)}</dd></dl>}
  {can('org.wallet.read') && <section><h4>{copy.packages}</h4>{data.packages.map(item => <div key={item.package_id}><p>{item.plan_name} · {item.package_id} · {organizationLabel(copy, 'Status', item.status)} · {formatBalance(item.points_remaining_microcredits, language)}</p><ul>{item.components?.map(component => <li key={component.component_id}>{component.component_id} · {organizationLabel(copy, 'Component', component.component_type)} · {organizationLabel(copy, 'Status', component.status)} · {formatBalance(component.available_microcredits, language)}</li>)}</ul></div>)}</section>}
  {can('org.entitlements.purchase') && <section>{data.capabilities && <p role="status">{data.capabilities.test_only ? copy.testCollection : data.capabilities.formal_collection_admitted ? copy.formalCollection : copy.collectionUnavailable}</p>}{!busy && !purchaseProducts.length && <p>{copy.noPurchaseChannel}</p>}<OrganizationActionForm disabled={!purchaseProducts.length} client={client} owner={owner} copy={copy} scope="organization-purchase" title={copy.purchase} fields={[{ name: 'product', options: purchaseProducts.map(product => ({ value: product.product_id, label: `${localizedProductDisplayName(product, language)} · ${formatFen(product.amount_fen, language)}` })) }, { name: 'channel', options: values => organizationPaymentChannels(purchaseProducts.find(product => product.product_id === values.product), data.capabilities).map(value => ({ value, label: organizationLabel(copy, 'Channel', value) })) }]} prepare={values => { const product = purchaseProducts.find(item => item.product_id === values.product); const channel = effectivePaymentChannel(organizationPaymentChannels(product, data.capabilities), values.channel); if (!product || !channel || channel !== values.channel) throw new Error('INVALID_ORGANIZATION_INPUT'); return { product_id: product.product_id, channel }; }} action={(facts, key) => api.createOrder(org, facts, key)} onConfirmed={result => { if (ownsView(generation.current) && currentView.current.canPurchase) { setOrder(result); setOrderID(result.order_id); } }} />
   <form onSubmit={event => { event.preventDefault(); void query(); }}><label>{copy.order}<input value={orderID} onChange={event => setOrderID(event.target.value)} /></label><button disabled={busy || !orderID}>{copy.queryOrder}</button></form>
   {order && <div><p>{order.order_id} · {organizationLabel(copy, 'Status', order.status)} · {organizationLabel(copy, 'Status', order.fulfillment_status)} · {formatFen(order.amount_fen, language)}</p>{paymentURL && <a href={paymentURL} target="_blank" rel="noopener noreferrer">{copy.pay}</a>}{paymentQR && <img src={paymentQR} alt={copy.pay} />}</div>}
  </section>}
  {can('org.quota.read.self') && <section><h4>{copy.quota}</h4>{data.grants.map(grant => <div key={grant.grant_id}><p>{grant.member_user_id} · {grant.grant_id} · {organizationLabel(copy, 'Status', grant.status)} · {formatBalance(grant.available_microcredits, language)} · {formatBalance(grant.reserved_microcredits, language)}</p>{can('org.quota.revoke') && grant.status === 'ACTIVE' && <OrganizationActionForm client={client} owner={owner} copy={copy} scope={`quota-revoke:${grant.grant_id}`} confirmationTarget={`${org} · ${grant.member_user_id} · ${grant.grant_id}`} title={copy.revoke} fields={[{ name: 'reason' }]} prepare={values => { if (!values.reason.trim()) throw new Error('INVALID_ORGANIZATION_INPUT'); return { id: grant.grant_id, reason: values.reason.trim() }; }} action={(facts, key) => api.revokeGrant(org, facts.id, facts.reason, key)} onConfirmed={() => void refresh()} confirm />}</div>)}</section>}
  {can('org.quota.allocate') && <section><p>{copy.membersPageHint}</p>{memberPage.loading && <p role="status">{copy.loading}</p>}{memberPage.error && <p role="alert">{copy[memberPage.error]}</p>}<button type="button" disabled={memberPage.loading} onClick={() => void memberPage.refresh()}>{copy.refresh}</button><button type="button" disabled={memberPage.loading || !memberPage.nextCursor} onClick={() => void memberPage.loadMore()}>{copy.loadMoreMembers}</button><ul>{memberPage.items.map(member => <li key={member.user_id}>{member.display_name || member.user_id} · {member.user_id} · {member.version}</li>)}</ul><OrganizationActionForm client={client} owner={owner} copy={copy} scope="quota-allocation" title={copy.allocate} fields={[{ name: 'user' }, { name: 'package' }, { name: 'component' }, { name: 'componentType', options: ['BASE', 'BONUS'].map(value => ({ value, label: organizationLabel(copy, 'Component', value) })) }, { name: 'amount', type: 'text' }]} prepare={values => { const users = values.user.split(',').map(value => organizationID(value.trim())); if (!users.length || users.length > 200 || new Set(users).size !== users.length) throw new Error('INVALID_ORGANIZATION_INPUT'); const amount = integerMoney(values.amount); if (amount <= 0n || amount.toString() !== values.amount || !['BASE', 'BONUS'].includes(values.componentType)) throw new Error('INVALID_ORGANIZATION_INPUT'); return { items: users.map(user => { const member = memberPage.items.find(item => item.user_id === user); return { member_user_id: user, membership_version: organizationVersion(member?.version), package_id: organizationID(values.package), component_id: organizationID(values.component), component_type: values.componentType, amount_microcredits: amount.toString() }; }) }; }} action={(facts, key) => api.allocate(org, facts, key)} onConfirmed={result => { setBatch(result); void refresh(); }} />
   {batch && <div>{batch.items.some(item => item.status === 'FAILED') && <p role="alert">{copy.partial}</p>}<ul>{batch.items.map(item => <li key={item.index}>{item.index + 1} · {organizationLabel(copy, 'Status', item.status)} · {item.grant?.grant_id || (organizationLabel(copy, 'Code', item.code) === item.code ? item.code : `${organizationLabel(copy, 'Code', item.code)} (${item.code})`)}</li>)}</ul></div>}
  </section>}
 </section>;
}
