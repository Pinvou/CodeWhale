//! One temporary-model turn's validated DNS snapshot and HTTP constraints.
//! The engine owns inference; this app policy supplies only its egress boundary.

use std::fmt;
use std::net::{SocketAddr, ToSocketAddrs};
use std::sync::{Arc, OnceLock};
use std::time::Duration;

const DNS_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_DNS_ADDRESSES: usize = 32;
// A timeout cannot cancel an OS resolver. Hold the permit inside its blocking
// operation until it really exits, including when its awaiting turn is gone.
static DNS_BUDGET: OnceLock<Arc<tokio::sync::Semaphore>> = OnceLock::new();

pub(crate) struct PinnedEndpoint {
    origin: String,
    host: String,
    addresses: Vec<SocketAddr>,
}

pub(crate) struct RuntimeEgress {
    endpoints: Vec<PinnedEndpoint>,
}

impl fmt::Debug for RuntimeEgress {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("RuntimeEgress")
            .field("endpoint_count", &self.endpoints.len())
            .finish()
    }
}

impl RuntimeEgress {
    pub(crate) fn new(primary: PinnedEndpoint, vision: Option<PinnedEndpoint>) -> Arc<Self> {
        let mut endpoints = vec![primary];
        if let Some(vision) = vision {
            endpoints.push(vision);
        }
        Arc::new(Self { endpoints })
    }

    pub(crate) fn configure(
        &self,
        builder: reqwest::ClientBuilder,
        base_url: &str,
    ) -> Result<reqwest::ClientBuilder, &'static str> {
        let url = reqwest::Url::parse(base_url).map_err(|_| "model_endpoint_invalid")?;
        if !url.username().is_empty() || url.password().is_some() {
            return Err("model_endpoint_unprepared");
        }
        let endpoint = self
            .endpoints
            .iter()
            .find(|endpoint| endpoint.origin == url.origin().ascii_serialization())
            .ok_or("model_endpoint_unprepared")?;
        Ok(builder
            .https_only(true)
            .no_proxy()
            .redirect(reqwest::redirect::Policy::none())
            .dns_resolver(Arc::new(SnapshotResolver {
                host: endpoint.host.clone(),
                addresses: endpoint.addresses.clone(),
            })))
    }
}

struct SnapshotResolver {
    host: String,
    addresses: Vec<SocketAddr>,
}

impl reqwest::dns::Resolve for SnapshotResolver {
    fn resolve(&self, name: reqwest::dns::Name) -> reqwest::dns::Resolving {
        let matches = name
            .as_str()
            .trim_end_matches('.')
            .eq_ignore_ascii_case(&self.host);
        let addresses = self.addresses.clone();
        Box::pin(async move {
            if !matches {
                return Err(std::io::Error::other("model_endpoint_unprepared").into());
            }
            Ok(Box::new(addresses.into_iter()) as reqwest::dns::Addrs)
        })
    }
}

pub(crate) async fn prepare_endpoint(base_url: &str) -> Result<PinnedEndpoint, &'static str> {
    let url = reqwest::Url::parse(base_url).map_err(|_| "model_endpoint_invalid")?;
    if url.scheme() != "https" {
        return Err("model_endpoint_insecure");
    }
    if !url.username().is_empty() || url.password().is_some() {
        return Err("model_endpoint_invalid");
    }
    let host = url
        .host_str()
        .ok_or("model_endpoint_invalid")?
        .trim_start_matches('[')
        .trim_end_matches(']')
        .trim_end_matches('.')
        .to_ascii_lowercase();
    if host.is_empty() {
        return Err("model_endpoint_invalid");
    }
    let port = url
        .port_or_known_default()
        .ok_or("model_endpoint_invalid")?;
    let addresses = if let Ok(address) = host.parse::<std::net::IpAddr>() {
        vec![SocketAddr::new(address, port)]
    } else {
        let lookup_host = host.clone();
        bounded_dns_lookup(
            DNS_BUDGET
                .get_or_init(|| Arc::new(tokio::sync::Semaphore::new(4)))
                .clone(),
            DNS_TIMEOUT,
            move || {
                (lookup_host.as_str(), port)
                    .to_socket_addrs()
                    .map(|addresses| addresses.take(MAX_DNS_ADDRESSES + 1).collect::<Vec<_>>())
            },
        )
        .await?
    };
    if addresses.len() > MAX_DNS_ADDRESSES {
        return Err("model_endpoint_dns_capacity");
    }
    let mut addresses = super::temporary_model::validate_runtime_addresses(addresses)
        .map_err(|error| error.code)?;
    // reqwest installs the original URL's port while preserving TLS hostname/SNI.
    for address in &mut addresses {
        address.set_port(0);
    }
    Ok(PinnedEndpoint {
        origin: url.origin().ascii_serialization(),
        host,
        addresses,
    })
}

async fn bounded_dns_lookup<F>(
    budget: Arc<tokio::sync::Semaphore>,
    timeout: Duration,
    lookup: F,
) -> Result<Vec<SocketAddr>, &'static str>
where
    F: FnOnce() -> std::io::Result<Vec<SocketAddr>> + Send + 'static,
{
    let permit = budget
        .try_acquire_owned()
        .map_err(|_| "model_endpoint_dns_busy")?;
    let work = tokio::task::spawn_blocking(move || {
        let _permit = permit;
        lookup()
    });
    tokio::time::timeout(timeout, work)
        .await
        .map_err(|_| "model_endpoint_unresolved")?
        .map_err(|_| "model_endpoint_unresolved")?
        .map_err(|_| "model_endpoint_unresolved")
}

#[cfg(test)]
mod tests {
    use super::*;
    use reqwest::dns::Resolve as _;

    struct ReleaseLookup(Option<std::sync::mpsc::Sender<()>>);
    impl Drop for ReleaseLookup {
        fn drop(&mut self) {
            if let Some(release) = self.0.take() {
                let _ = release.send(());
            }
        }
    }

    #[tokio::test(start_paused = true)]
    async fn timed_out_or_cancelled_dns_retains_capacity_until_os_work_exits() {
        for cancel in [false, true] {
            let budget = Arc::new(tokio::sync::Semaphore::new(1));
            let (started, entered) = tokio::sync::oneshot::channel();
            let (release, wait) = std::sync::mpsc::channel();
            let mut release = ReleaseLookup(Some(release));
            let work = tokio::spawn(bounded_dns_lookup(budget.clone(), DNS_TIMEOUT, move || {
                let _ = started.send(());
                wait.recv().unwrap();
                Ok(vec!["1.1.1.1:443".parse().unwrap()])
            }));
            entered.await.unwrap();
            if cancel {
                work.abort();
                assert!(work.await.unwrap_err().is_cancelled());
            } else {
                tokio::time::advance(DNS_TIMEOUT).await;
                assert_eq!(work.await.unwrap(), Err("model_endpoint_unresolved"));
            }
            assert_eq!(budget.available_permits(), 0);
            assert_eq!(
                bounded_dns_lookup(budget.clone(), DNS_TIMEOUT, || panic!(
                    "must not start a fifth lookup"
                ))
                .await,
                Err("model_endpoint_dns_busy")
            );
            release.0.take().unwrap().send(()).unwrap();
            // Acquisition observes the blocking operation's actual permit Drop.
            drop(budget.acquire().await.unwrap());
            assert!(
                bounded_dns_lookup(budget.clone(), DNS_TIMEOUT, || Ok(vec![]))
                    .await
                    .is_ok()
            );
        }
    }

    #[tokio::test]
    async fn snapshot_resolver_never_falls_back_to_process_dns() {
        let resolver = SnapshotResolver {
            host: "owned.invalid".into(),
            addresses: vec!["1.1.1.1:0".parse().unwrap(), "8.8.8.8:0".parse().unwrap()],
        };
        let actual = resolver
            .resolve("owned.invalid".parse().unwrap())
            .await
            .unwrap()
            .collect::<Vec<_>>();
        assert_eq!(actual, resolver.addresses);
        assert!(
            resolver
                .resolve("localhost".parse().unwrap())
                .await
                .is_err()
        );
    }

    #[tokio::test]
    async fn prepared_transport_refuses_unprepared_origins_and_insecure_requests() {
        let endpoint = prepare_endpoint("https://1.1.1.1/v1").await.unwrap();
        let policy = RuntimeEgress::new(endpoint, None);
        assert!(
            policy
                .configure(reqwest::Client::builder(), "https://1.1.1.1:444/v1")
                .is_err()
        );
        assert!(
            policy
                .configure(reqwest::Client::builder(), "http://1.1.1.1/v1")
                .is_err()
        );
        let client = policy
            .configure(reqwest::Client::builder(), "https://1.1.1.1/v1")
            .unwrap()
            .build()
            .unwrap();
        let error = client.get("http://127.0.0.1:9/").send().await.unwrap_err();
        assert!(error.is_builder());
        for base in [
            "https://127.0.0.1/",
            "https://[::1]/",
            "https://user:secret@1.1.1.1/",
        ] {
            assert!(prepare_endpoint(base).await.is_err());
        }
    }

    #[tokio::test]
    #[ignore = "run through model-egress-transport-fixture.mjs for isolated TLS/proxy listeners"]
    async fn isolated_tls_snapshot_preserves_hostname_and_blocks_proxy_and_redirect() {
        let fixture: serde_json::Value = serde_json::from_slice(
            &std::fs::read(
                std::env::var_os("PINVOU_EGRESS_TRANSPORT_FIXTURE")
                    .expect("isolated fixture manifest"),
            )
            .unwrap(),
        )
        .unwrap();
        let primary = fixture["primary_url"].as_str().unwrap();
        let vision = fixture["vision_url"].as_str().unwrap();
        let endpoint = |base: &str| {
            let url = reqwest::Url::parse(base).unwrap();
            assert!(matches!(
                url.host_str(),
                Some("primary.invalid" | "vision.invalid")
            ));
            // Fixture-only pins deliberately use loopback. Production admission
            // cannot produce these pins; private/mixed IP rejection is separate.
            PinnedEndpoint {
                origin: url.origin().ascii_serialization(),
                host: url.host_str().unwrap().into(),
                addresses: vec!["127.0.0.1:0".parse().unwrap()],
            }
        };
        let policy = RuntimeEgress::new(endpoint(primary), Some(endpoint(vision)));
        let ca =
            reqwest::Certificate::from_pem(fixture["ca_pem"].as_str().unwrap().as_bytes()).unwrap();
        for base in [primary, vision] {
            for http1 in [false, true] {
                let builder = reqwest::Client::builder()
                    .add_root_certificate(ca.clone())
                    .timeout(Duration::from_secs(5));
                let builder = if http1 { builder.http1_only() } else { builder };
                let client = policy.configure(builder, base).unwrap().build().unwrap();
                let response = client.get(format!("{base}/ok")).send().await.unwrap();
                assert_eq!(response.status(), reqwest::StatusCode::OK);
                assert_eq!(response.text().await.unwrap(), "owned TLS response");
                let response = client.get(format!("{base}/redirect")).send().await.unwrap();
                assert_eq!(response.status(), reqwest::StatusCode::FOUND);
                assert!(
                    client
                        .get("https://unprepared.invalid/")
                        .send()
                        .await
                        .is_err()
                );
            }
        }
    }
}
