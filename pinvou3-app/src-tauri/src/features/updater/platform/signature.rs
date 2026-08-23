use std::path::Path;
use std::process::Command;

const SIGNER_IDENTITY_ENV: &str = "PINVOU_UPDATE_SIGNER_IDENTITY";

fn signer_identity() -> Result<String, String> {
    std::env::var(SIGNER_IDENTITY_ENV)
        .ok()
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
        .ok_or_else(|| format!("更新安装门禁失败：缺少受信配置 {SIGNER_IDENTITY_ENV}"))
}

#[cfg(target_os = "windows")]
pub(crate) fn verify_package_signature(
    _package: &Path,
    installer: Option<&Path>,
) -> Result<(), String> {
    let installer = installer.ok_or_else(|| "更新安装门禁失败：缺少待验证安装器".to_string())?;
    let signer = signer_identity()?.replace(' ', "").to_ascii_uppercase();
    let script = "$s=Get-AuthenticodeSignature -LiteralPath $args[0]; $t=(($s.SignerCertificate.Thumbprint -replace ' ','').ToUpperInvariant()); if($s.Status -ne 'Valid' -or $t -ne $args[1]){exit 19}";
    let status = Command::new("powershell.exe")
        .args(["-NoProfile", "-NonInteractive", "-Command", script])
        .arg(installer)
        .arg(&signer)
        .status()
        .map_err(|error| format!("更新安装门禁失败：无法验证安装器签名：{error}"))?;
    status
        .success()
        .then_some(())
        .ok_or_else(|| "更新安装门禁失败：安装器签名或签名者不受信".to_string())
}

#[cfg(target_os = "macos")]
pub(crate) fn verify_package_signature(
    package: &Path,
    _installer: Option<&Path>,
) -> Result<(), String> {
    let identity = signer_identity()?;
    let output = Command::new("codesign")
        .args(["--verify", "--deep", "--strict", "--verbose=4"])
        .arg(package)
        .output()
        .map_err(|error| format!("更新安装门禁失败：无法验证包签名：{error}"))?;
    let evidence = String::from_utf8_lossy(&output.stderr);
    (output.status.success() && evidence.contains(&identity))
        .then_some(())
        .ok_or_else(|| "更新安装门禁失败：包签名或签名者不受信".to_string())
}

#[cfg(target_os = "linux")]
pub(crate) fn verify_package_signature(
    package: &Path,
    _installer: Option<&Path>,
) -> Result<(), String> {
    let identity = signer_identity()?;
    let output = Command::new("dpkg-sig")
        .arg("--verify")
        .arg(package)
        .output()
        .map_err(|error| format!("更新安装门禁失败：无法验证包签名：{error}"))?;
    let evidence = format!(
        "{}{}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    (output.status.success() && evidence.contains(&identity))
        .then_some(())
        .ok_or_else(|| "更新安装门禁失败：包签名或签名者不受信".to_string())
}

#[cfg(not(any(target_os = "windows", target_os = "macos", target_os = "linux")))]
pub(crate) fn verify_package_signature(
    _package: &Path,
    _installer: Option<&Path>,
) -> Result<(), String> {
    Err("更新安装门禁失败：当前平台不支持包签名验证".to_string())
}
