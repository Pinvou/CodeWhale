//! Test-only transport for exercising HTTPS card inventories over local fixtures.
//! Hash checks and all caller policies still execute in fetch_script_content.
use super::*;
use std::cell::RefCell;
use std::marker::PhantomData;
use std::rc::Rc;

thread_local! {
    static ORIGIN: RefCell<Option<String>> = const { RefCell::new(None) };
}

pub struct Guard {
    previous: Option<String>,
    // A thread-local override must be restored on the thread that installed it.
    _thread: PhantomData<Rc<()>>,
}

pub fn install(origin: &str) -> Guard {
    let parsed = reqwest::Url::parse(origin).unwrap();
    assert_eq!(parsed.scheme(), "https");
    assert_eq!(parsed.host_str(), Some("127.0.0.1"));
    assert!(parsed.port().is_some_and(|port| port > 0));
    assert_eq!(parsed.path(), "/");
    assert!(parsed.username().is_empty() && parsed.password().is_none());
    assert!(parsed.query().is_none() && parsed.fragment().is_none());
    Guard {
        previous: ORIGIN
            .with(|current| current.replace(Some(parsed.origin().ascii_serialization()))),
        _thread: PhantomData,
    }
}

impl Drop for Guard {
    fn drop(&mut self) {
        ORIGIN.with(|current| current.replace(self.previous.take()));
    }
}

pub(super) fn get(url: &str) -> Option<Result<String, ScriptError>> {
    let origin = ORIGIN.with(|current| current.borrow().clone())?;
    Some((|| {
        let mut parsed = reqwest::Url::parse(url)
            .map_err(|error| ScriptError::new(format!("fixture URL is invalid: {error}")))?;
        if parsed.origin().ascii_serialization() != origin
            || !parsed.username().is_empty()
            || parsed.password().is_some()
        {
            return Err(ScriptError::new(
                "fixture refused an unexpected asset origin",
            ));
        }
        parsed.set_scheme("http").unwrap();
        reqwest::blocking::Client::builder()
            .no_proxy()
            .timeout(Duration::from_secs(2))
            .redirect(reqwest::redirect::Policy::none())
            .build()
            .and_then(|client| client.get(parsed).send())
            .and_then(|response| response.error_for_status())
            .and_then(|response| response.text())
            .map_err(|error| ScriptError::new(format!("loopback fixture fetch failed: {error}")))
    })())
}
