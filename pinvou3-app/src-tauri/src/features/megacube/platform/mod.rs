#[cfg(unix)]
pub(super) use crate::platform::os::local_socket::connect_timeout;
#[cfg(target_os = "linux")]
pub(super) use crate::platform::os::local_socket::try_connect;

#[cfg(all(test, target_os = "linux"))]
pub(crate) mod test_fixture;
