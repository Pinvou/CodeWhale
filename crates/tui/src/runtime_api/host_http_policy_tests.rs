use super::*;
use crate::client::DeepSeekClient;
use crate::config::ModelHttpPolicy;
use crate::vision::tools::ImageAnalyzeTool;

#[derive(Debug, Default)]
struct RecordingPolicy(std::sync::Mutex<Vec<String>>);

impl ModelHttpPolicy for RecordingPolicy {
    fn configure(
        &self,
        builder: reqwest::ClientBuilder,
        base_url: &str,
    ) -> Result<reqwest::ClientBuilder> {
        let url =
            reqwest::Url::parse(base_url).map_err(|_| anyhow::anyhow!("invalid fixture route"))?;
        anyhow::ensure!(
            url.host_str() == Some("127.0.0.1") && url.port() == Some(9),
            "unexpected fixture route"
        );
        self.0.lock().unwrap().push(base_url.to_owned());
        Ok(builder.no_proxy())
    }
}

fn disk_config(provider: &str, vision: bool) -> String {
    let mut text = format!(
        "provider = '{provider}'\napi_key = 'fixture-primary-key'\nbase_url = 'http://127.0.0.1:9/primary/v1'\ndefault_text_model = 'deepseek-v4-pro'\n[providers.openai]\napi_key = 'fixture-openai-key'\nbase_url = 'http://127.0.0.1:9/second/v1'\nmodel = 'gpt-4.1'\n"
    );
    if vision {
        text.push_str("[vision_model]\nmodel = 'fixture-vision'\napi_key = 'fixture-vision-key'\nbase_url = 'http://127.0.0.1:9/vision/v1'\n");
    }
    text
}

struct Fixture {
    _root: tempfile::TempDir,
    state: RuntimeApiState,
    server: tokio::task::JoinHandle<()>,
    addr: SocketAddr,
    file: PathBuf,
    primary: Arc<RecordingPolicy>,
    vision: Arc<RecordingPolicy>,
}

impl Drop for Fixture {
    fn drop(&mut self) {
        self.state.runtime_threads.shutdown();
        self.state.task_manager.shutdown();
        self.server.abort();
    }
}

impl Fixture {
    async fn new() -> Result<Self> {
        let root = tempfile::tempdir()?;
        let file = root.path().join("config.toml");
        fs::write(&file, disk_config("deepseek", true))?;
        let primary = Arc::new(RecordingPolicy::default());
        let vision = Arc::new(RecordingPolicy::default());
        let mut config = Config::load(Some(file.clone()), None)?;
        config.model_http_policy = Some(primary.clone());
        config.vision_model_http_policy = Some(vision.clone());
        let (probe, state) = tokio::sync::oneshot::channel();
        let (addr, _, server) = spawn_test_server_with_root_token_mobile_workspace_and_overrides(
            root.path().join("server"),
            root.path().join("sessions"),
            None,
            false,
            root.path().join("workspace"),
            TestServerOverrides {
                config: Some(config),
                config_path: Some(file.clone()),
                state_probe: Some(probe),
                ..TestServerOverrides::default()
            },
        )
        .await?
        .ok_or_else(|| anyhow::anyhow!("required loopback fixture unavailable"))?;
        let state = tokio::time::timeout(Duration::from_secs(10), state).await??;
        Ok(Self {
            _root: root,
            state,
            server,
            addr,
            file,
            primary,
            vision,
        })
    }

    async fn reload(&self) -> Result<StatusCode> {
        Ok(crate::tls::reqwest_client()
            .post(format!("http://{}/v1/config/reload", self.addr))
            .send()
            .await?
            .status())
    }

    fn assert_policies_and_builders(&self, expected_primary_path: &str) -> Result<()> {
        let primary: Arc<dyn ModelHttpPolicy> = self.primary.clone();
        let vision: Arc<dyn ModelHttpPolicy> = self.vision.clone();
        let config = self.state.config.read().clone();
        let runtime = self.state.runtime_threads.read_config();
        for snapshot in [&config, &*runtime] {
            assert!(Arc::ptr_eq(
                snapshot.model_http_policy.as_ref().unwrap(),
                &primary
            ));
            assert!(Arc::ptr_eq(
                snapshot.vision_model_http_policy.as_ref().unwrap(),
                &vision
            ));
        }
        drop(runtime);
        self.primary.0.lock().unwrap().clear();
        self.vision.0.lock().unwrap().clear();
        let model = DeepSeekClient::new(&config)?;
        let vision_config = config.vision_model_config().unwrap();
        let vision_url = vision_config.base_url.clone().unwrap();
        let _tool = ImageAnalyzeTool::new_with_route_client(vision_config, Some(model));
        assert_eq!(
            *self.primary.0.lock().unwrap(),
            vec![format!("http://127.0.0.1:9{expected_primary_path}"); 2],
            "primary and HTTP1 must both apply policy to the newly selected route"
        );
        assert_eq!(*self.vision.0.lock().unwrap(), vec![vision_url]);
        Ok(())
    }
}

#[tokio::test]
async fn forkguard_host_http_policy_survives_reload_switch_and_vision_reenable() -> Result<()> {
    let _env = crate::test_support::lock_test_env();
    let fixture = Fixture::new().await?;
    assert_eq!(fixture.reload().await?, StatusCode::OK);
    fixture.assert_policies_and_builders("/primary/v1")?;
    let (status, body) = post_switch_provider(
        &crate::tls::reqwest_client(),
        &fixture.addr,
        "openai",
        &json!({}),
    )
    .await;
    assert_eq!(status, StatusCode::OK, "{body}");
    fixture.assert_policies_and_builders("/second/v1")?;
    fs::write(&fixture.file, disk_config("openai", false))?;
    assert_eq!(fixture.reload().await?, StatusCode::OK);
    assert!(fixture.state.config.read().vision_model.is_none());
    fs::write(&fixture.file, disk_config("openai", true))?;
    assert_eq!(fixture.reload().await?, StatusCode::OK);
    fixture.assert_policies_and_builders("/second/v1")?;
    fs::write(&fixture.file, "[malformed")?;
    assert!(fixture.reload().await?.is_server_error());
    fixture.assert_policies_and_builders("/second/v1")?;
    fs::write(&fixture.file, disk_config("openai", true))?;
    assert_eq!(fixture.reload().await?, StatusCode::OK);
    fixture.assert_policies_and_builders("/second/v1")?;
    Ok(())
}
