import React, { useEffect, useMemo, useRef, useState } from 'react';
import { organizationCopy } from '../../shared/i18n.js';
import { OrganizationClient, organizationID, organizationVersion, organizationPaymentChannels } from './organization-client.js';
import { OrganizationActionForm } from './OrganizationActionForm.jsx';
import { hasPermission } from './authorization-ui.js';
import { formatBalance } from './balance-view.js';
import { integerMoney, formatFen, safePaymentURL, safePaymentQR } from './finance-contract.js';
import { isPayablePaymentOrder, effectivePaymentChannel } from './order-view.js';
import { localizedProductDisplayName } from './localized-product-display-name.js';

export function OrganizationFinanceWorkspace({ client, me, language }) {
 const copy = organizationCopy[language], org = me.active_context.organization_id;
 const api = useMemo(() => new OrganizationClient(client), [client]);
 const owner = `${me.user_id}:${org}:${me.active_context.context_version}:${me.active_context.organization_authorization_version}:${client.financeSessionEpoch || 0}`;
 const [data, setData] = useState({ packages: [], grants: [], members: [], products: [] });
 const [order, setOrder] = useState(null), [orderID, setOrderID] = useState(''), [batch, setBatch] = useState(null);
 const [error, setError] = useState(''), [busy, setBusy] = useState(false), [now, setNow] = useState(Date.now());
 const generation = useRef(0), can = permission => hasPermission(me, permission);
 const load = async (stamp, signal) => {
  const [balance, packages, grants, members, products, capabilities] = await Promise.all([
   can('org.wallet.read') ? api.balance(org, signal) : null,
   can('org.wallet.read') ? api.packages(org, signal) : [],
   can('org.quota.read.self') ? api.grants(org, signal) : [],
   can('org.quota.allocate') ? api.quotaMembers(org, signal) : [],
   can('org.entitlements.purchase') ? api.products(org, signal) : [],
   can('org.entitlements.purchase') ? api.capabilities(org, signal) : null,
  ]);
  if (stamp === generation.current) setData({ balance, packages, grants, members, products, capabilities });
 };
 const refresh = async () => { const stamp = ++generation.current; setBusy(true); setError(''); try { await load(stamp); } catch { if (stamp === generation.current) setError(copy.unavailable); } finally { if (stamp === generation.current) setBusy(false); } };
 useEffect(() => { const stamp = ++generation.current, controller = new AbortController(); setData({ packages: [], grants: [], members: [], products: [] }); setOrder(null); setOrderID(''); setBatch(null); setError(''); setBusy(true); void load(stamp, controller.signal).catch(reason => { if (stamp === generation.current && reason.name !== 'AbortError') setError(copy.unavailable); }).finally(() => { if (stamp === generation.current) setBusy(false); }); return () => { generation.current++; controller.abort(); }; }, [api, owner]); // eslint-disable-line react-hooks/exhaustive-deps
 useEffect(() => { const interval = globalThis.setInterval(() => setNow(Date.now()), 1000); return () => globalThis.clearInterval(interval); }, []);
 const query = async () => { if (busy) return; const stamp = generation.current; setBusy(true); setError(''); try { const result = await api.order(org, organizationID(orderID)); if (stamp === generation.current) setOrder(result); } catch { if (stamp === generation.current) setError(copy.unavailable); } finally { if (stamp === generation.current) setBusy(false); } };
 const purchaseProducts = data.products.filter(product => organizationPaymentChannels(product, data.capabilities).length > 0);
 const paymentURL = isPayablePaymentOrder(order, now) ? safePaymentURL(order?.payment_url) : '';
 const paymentQR = isPayablePaymentOrder(order, now) ? safePaymentQR(order?.qr_data) : '';
 return <section><h3>{copy.balance}</h3><button disabled={busy} onClick={() => void refresh()}>{copy.refresh}</button>{error && <p role="alert">{error}</p>}{busy && <p role="status">{copy.loading}</p>}
  {data.balance && <dl><dt>{copy.available}</dt><dd>{formatBalance(data.balance.available_microcredits, language)}</dd><dt>{copy.reserved}</dt><dd>{formatBalance(data.balance.reserved_microcredits, language)}</dd></dl>}
  {can('org.wallet.read') && <section><h4>{copy.packages}</h4>{data.packages.map(item => <div key={item.package_id}><p>{item.plan_name} · {item.package_id} · {item.status} · {formatBalance(item.points_remaining_microcredits, language)}</p><ul>{item.components?.map(component => <li key={component.component_id}>{component.component_id} · {component.component_type} · {component.status} · {formatBalance(component.available_microcredits, language)}</li>)}</ul></div>)}</section>}
  {can('org.entitlements.purchase') && <section>{data.capabilities && <p role="status">{data.capabilities.test_only ? copy.testCollection : data.capabilities.formal_collection_admitted ? copy.formalCollection : copy.collectionUnavailable}</p>}{!busy && !purchaseProducts.length && <p>{copy.noPurchaseChannel}</p>}<OrganizationActionForm disabled={!purchaseProducts.length} client={client} owner={owner} copy={copy} scope="organization-purchase" title={copy.purchase} fields={[{ name: 'product', options: purchaseProducts.map(product => ({ value: product.product_id, label: `${localizedProductDisplayName(product, language)} · ${formatFen(product.amount_fen, language)}` })) }, { name: 'channel', options: values => organizationPaymentChannels(purchaseProducts.find(product => product.product_id === values.product), data.capabilities).map(value => ({ value, label: value })) }]} prepare={values => { const product = purchaseProducts.find(item => item.product_id === values.product); const channel = effectivePaymentChannel(organizationPaymentChannels(product, data.capabilities), values.channel); if (!product || !channel || channel !== values.channel) throw new Error('INVALID_ORGANIZATION_INPUT'); return { product_id: product.product_id, channel }; }} action={(facts, key) => api.createOrder(org, facts, key)} onConfirmed={result => { setOrder(result); setOrderID(result.order_id); }} />
   <form onSubmit={event => { event.preventDefault(); void query(); }}><label>{copy.order}<input value={orderID} onChange={event => setOrderID(event.target.value)} /></label><button disabled={busy || !orderID}>{copy.queryOrder}</button></form>
   {order && <div><p>{order.order_id} · {order.status} · {order.fulfillment_status} · {formatFen(order.amount_fen, language)}</p>{paymentURL && <a href={paymentURL} target="_blank" rel="noopener noreferrer">{copy.pay}</a>}{paymentQR && <img src={paymentQR} alt={copy.pay} />}</div>}
  </section>}
  {can('org.quota.read.self') && <section><h4>{copy.quota}</h4>{data.grants.map(grant => <div key={grant.grant_id}><p>{grant.member_user_id} · {grant.grant_id} · {grant.status} · {formatBalance(grant.available_microcredits, language)} · {formatBalance(grant.reserved_microcredits, language)}</p>{can('org.quota.revoke') && grant.status === 'ACTIVE' && <OrganizationActionForm client={client} owner={owner} copy={copy} scope={`quota-revoke:${grant.grant_id}`} title={copy.revoke} fields={[{ name: 'reason' }]} prepare={values => { if (!values.reason.trim()) throw new Error('INVALID_ORGANIZATION_INPUT'); return { id: grant.grant_id, reason: values.reason.trim() }; }} action={(facts, key) => api.revokeGrant(org, facts.id, facts.reason, key)} onConfirmed={() => void refresh()} confirm />}</div>)}</section>}
  {can('org.quota.allocate') && <section><ul>{data.members.map(member => <li key={member.user_id}>{member.display_name || member.user_id} · {member.user_id} · {member.version}</li>)}</ul><OrganizationActionForm client={client} owner={owner} copy={copy} scope="quota-allocation" title={copy.allocate} fields={[{ name: 'user' }, { name: 'package' }, { name: 'component' }, { name: 'componentType', options: ['BASE', 'BONUS', 'PLAN'].map(value => ({ value, label: value })) }, { name: 'amount', type: 'text' }]} prepare={values => { const users = values.user.split(',').map(value => organizationID(value.trim())); if (!users.length || users.length > 200 || new Set(users).size !== users.length) throw new Error('INVALID_ORGANIZATION_INPUT'); const amount = integerMoney(values.amount); if (amount <= 0n || amount.toString() !== values.amount || !['BASE', 'BONUS', 'PLAN'].includes(values.componentType)) throw new Error('INVALID_ORGANIZATION_INPUT'); return { items: users.map(user => { const member = data.members.find(item => item.user_id === user); return { member_user_id: user, membership_version: organizationVersion(member?.version), package_id: organizationID(values.package), component_id: organizationID(values.component), component_type: values.componentType, amount_microcredits: amount.toString() }; }) }; }} action={(facts, key) => api.allocate(org, facts, key)} onConfirmed={result => { setBatch(result); void refresh(); }} />
   {batch && <div>{batch.items.some(item => item.status === 'FAILED') && <p role="alert">{copy.partial}</p>}<ul>{batch.items.map(item => <li key={item.index}>{item.index + 1} · {item.status} · {item.grant?.grant_id || item.code}</li>)}</ul></div>}
  </section>}
 </section>;
}
