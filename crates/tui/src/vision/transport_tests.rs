//! Real loopback TLS and ALPN evidence; no provider calls or static private key.
use super::*;
use crate::test_support::{EnvVarGuard, lock_test_env};
use std::io::{Read, Write};
use std::sync::{
    Arc,
    atomic::{AtomicBool, Ordering},
};

type ObservedTlsRequest = (Option<Vec<u8>>, String);

struct Endpoint {
    url: String,
    stop: Arc<AtomicBool>,
    worker: Option<std::thread::JoinHandle<std::io::Result<ObservedTlsRequest>>>,
}

impl Endpoint {
    fn start(cert: &rcgen::CertifiedKey<rcgen::KeyPair>) -> Self {
        crate::tls::ensure_rustls_crypto_provider();
        let mut tls = rustls::ServerConfig::builder()
            .with_no_client_auth()
            .with_single_cert(
                vec![cert.cert.der().clone()],
                rustls::pki_types::PrivatePkcs8KeyDer::from(cert.signing_key.serialize_der())
                    .into(),
            )
            .unwrap();
        tls.alpn_protocols = vec![b"h2".to_vec(), b"http/1.1".to_vec()];
        let listener = std::net::TcpListener::bind((std::net::Ipv4Addr::LOCALHOST, 0)).unwrap();
        listener.set_nonblocking(true).unwrap();
        let url = format!(
            "https://127.0.0.1:{}",
            listener.local_addr().unwrap().port()
        );
        let stop = Arc::new(AtomicBool::new(false));
        let flag = stop.clone();
        let worker = std::thread::spawn(move || {
            let deadline = std::time::Instant::now() + Duration::from_secs(3);
            let socket = loop {
                match listener.accept() {
                    Ok((socket, _)) => break socket,
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        if flag.load(Ordering::Acquire) || std::time::Instant::now() >= deadline {
                            return Err(std::io::Error::new(
                                std::io::ErrorKind::TimedOut,
                                "owned TLS accept ended",
                            ));
                        }
                        std::thread::sleep(Duration::from_millis(5));
                    }
                    Err(error) => return Err(error),
                }
            };
            // Windows accepts inherit the listener's nonblocking mode.
            socket.set_nonblocking(false)?;
            socket.set_read_timeout(Some(Duration::from_secs(2)))?;
            socket.set_write_timeout(Some(Duration::from_secs(2)))?;
            let mut tls = rustls::StreamOwned::new(
                rustls::ServerConnection::new(Arc::new(tls)).unwrap(),
                socket,
            );
            while tls.conn.is_handshaking() {
                tls.conn.complete_io(&mut tls.sock)?;
            }
            let protocol = tls.conn.alpn_protocol().map(<[u8]>::to_vec);
            if protocol.as_deref() == Some(b"h2") {
                // ALPN is the evidence here, not a hand-written HTTP/2 server.
                return Ok((protocol, String::new()));
            }
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") && request.len() < 16_384 {
                let mut byte = [0];
                if tls.read(&mut byte)? == 0 {
                    break;
                }
                request.push(byte[0]);
            }
            tls.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")?;
            tls.flush()?;
            Ok((protocol, String::from_utf8(request).unwrap()))
        });
        Self {
            url,
            stop,
            worker: Some(worker),
        }
    }

    fn finish(&mut self) -> std::io::Result<ObservedTlsRequest> {
        self.worker
            .take()
            .unwrap()
            .join()
            .expect("TLS worker panicked")
    }
}

impl Drop for Endpoint {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
    }
}

#[derive(Debug)]
struct LocalTransport {
    force_h2: bool,
}
impl crate::config::ModelHttpPolicy for LocalTransport {
    fn configure(
        &self,
        builder: reqwest::ClientBuilder,
        _: &str,
    ) -> anyhow::Result<reqwest::ClientBuilder> {
        let builder = builder.no_proxy();
        Ok(if self.force_h2 {
            builder.http2_prior_knowledge()
        } else {
            builder
        })
    }
}

fn vision_transport(url: &str, matched: bool, force_h2: bool) -> ImageAnalyzeTool {
    let config = crate::config::Config {
        provider: Some("deepseek".into()),
        api_key: Some("owned-loopback-key".into()),
        base_url: Some(url.into()),
        request_idempotency_header: Some("idempotency-key".into()),
        ..Default::default()
    };
    ImageAnalyzeTool::new_with_http_policy(
        VisionModelConfig {
            model: "owned-vision".into(),
            api_key: config.api_key.clone(),
            base_url: config.base_url.clone(),
        },
        matched.then(|| DeepSeekClient::new(&config).unwrap()),
        Some(Arc::new(LocalTransport { force_h2 })),
    )
}

fn transport_for_lane(url: &str, lane: &str) -> reqwest::Client {
    match lane {
        "primary" | "http1" => {
            let config = crate::config::Config {
                provider: Some("deepseek".into()),
                api_key: Some("owned-loopback-key".into()),
                base_url: Some(url.into()),
                model_http_policy: Some(Arc::new(LocalTransport { force_h2: false })),
                ..Default::default()
            };
            let route = DeepSeekClient::new(&config).unwrap();
            if lane == "http1" {
                route.http1_fallback_client().clone()
            } else {
                route.http_client.clone()
            }
        }
        _ => vision_transport(url, lane == "matching-vision", false)
            .client
            .unwrap(),
    }
}

#[tokio::test]
async fn forkguard_host_http_policy_vision_inherits_extra_roots_without_disabling_verification() {
    let _lock = lock_test_env();
    let _http1 = EnvVarGuard::set("CODEWHALE_FORCE_HTTP1", "1");
    let cert = rcgen::generate_simple_self_signed(vec!["127.0.0.1".into()]).unwrap();
    let root = tempfile::tempdir().unwrap();
    let second = rcgen::generate_simple_self_signed(vec!["other.invalid".into()]).unwrap();
    for lane in ["primary", "http1", "vision", "matching-vision"] {
        for (name, bytes, trusted) in [
            ("extra.pem", cert.cert.pem().into_bytes(), true),
            ("extra.der", cert.cert.der().to_vec(), true),
            (
                "bundle.pem",
                format!("{}{}", second.cert.pem(), cert.cert.pem()).into_bytes(),
                true,
            ),
            ("invalid.pem", b"not a certificate".to_vec(), false),
            ("invalid.der", cert.cert.der()[..32].to_vec(), false),
        ] {
            let path = root.path().join(name);
            std::fs::write(&path, bytes).unwrap();
            let _roots = EnvVarGuard::set("SSL_CERT_FILE", &path);
            let mut endpoint = Endpoint::start(&cert);
            let client = transport_for_lane(&endpoint.url, lane);
            let response = client
                .get(&endpoint.url)
                .timeout(Duration::from_secs(2))
                .send()
                .await;
            if trusted {
                let response = response.unwrap_or_else(|error| {
                    panic!("TLS request failed for {name}, lane={lane}: {error:?}")
                });
                assert_eq!(response.text().await.unwrap(), "ok", "{name}, lane={lane}");
                let (protocol, request) = endpoint.finish().unwrap();
                assert_ne!(protocol.as_deref(), Some(b"h2".as_slice()));
                assert!(request.starts_with("GET / HTTP/1.1\r\n"));
            } else {
                assert!(
                    response.unwrap_err().is_connect(),
                    "untrusted TLS must fail closed"
                );
                assert!(
                    endpoint.finish().is_err(),
                    "TLS peer must reject the bad trust source"
                );
            }
        }
    }
}

#[tokio::test]
async fn forkguard_host_http_policy_vision_honors_http1_alias_and_later_host_override() {
    let _lock = lock_test_env();
    let cert = rcgen::generate_simple_self_signed(vec!["127.0.0.1".into()]).unwrap();
    let root = tempfile::tempdir().unwrap();
    let path = root.path().join("root.pem");
    std::fs::write(&path, cert.cert.pem()).unwrap();
    let _roots = EnvVarGuard::set("SSL_CERT_FILE", &path);
    for (primary, legacy, override_h2, expected_h2) in [
        (None, None, false, true),
        (Some("1"), None, false, false),
        (None, Some("yes"), false, false),
        (Some("0"), Some("1"), false, true),
        (Some("1"), None, true, true),
    ] {
        let _primary = primary
            .map(|v| EnvVarGuard::set("CODEWHALE_FORCE_HTTP1", v))
            .unwrap_or_else(|| EnvVarGuard::remove("CODEWHALE_FORCE_HTTP1"));
        let _legacy = legacy
            .map(|v| EnvVarGuard::set("DEEPSEEK_FORCE_HTTP1", v))
            .unwrap_or_else(|| EnvVarGuard::remove("DEEPSEEK_FORCE_HTTP1"));
        let mut endpoint = Endpoint::start(&cert);
        let tool = vision_transport(&endpoint.url, false, override_h2);
        let response = tool
            .client
            .as_ref()
            .unwrap()
            .get(&endpoint.url)
            .timeout(Duration::from_secs(2))
            .send()
            .await;
        if !expected_h2 {
            assert_eq!(response.unwrap().text().await.unwrap(), "ok");
        }
        let (protocol, _) = endpoint.finish().unwrap();
        assert_eq!(
            protocol.as_deref() == Some(b"h2".as_slice()),
            expected_h2,
            "primary={primary:?} legacy={legacy:?} host_h2={override_h2}"
        );
    }
}
