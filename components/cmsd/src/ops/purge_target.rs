//! Resolve a destructive native target before stopping, then verify the same
//! directory and deployment immediately before deletion. Never widen a link
//! deletion into deletion of its resolved target.
use super::*;
use std::os::unix::fs::MetadataExt;

#[derive(Debug, PartialEq, Eq)]
struct Identity {
    canonical: PathBuf,
    directory: (u64, u64),
    entry: (u64, u64),
    artifact: (u64, u64),
    linked_entry: bool,
}

pub(super) struct Target {
    path: PathBuf,
    artifact: String,
    home: PathBuf,
    identity: Option<Identity>,
}

fn observe(path: &Path, artifact: &str, home: &Path) -> Result<Option<Identity>, String> {
    let canonical = match path.canonicalize() {
        Ok(value) => value,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(format!("不能核实权重目录，保留原资源：{error}")),
    };
    let home = home
        .canonicalize()
        .map_err(|_| "不能核实用户主目录，保留原资源".to_owned())?;
    if canonical.parent().is_none() || home.starts_with(&canonical) {
        return Err(format!(
            "拒绝删除危险路径 {}：真实目录是根、用户主目录或其祖先",
            path.display()
        ));
    }
    let directory = std::fs::metadata(&canonical).map_err(|e| e.to_string())?;
    let entry = std::fs::symlink_metadata(path).map_err(|e| e.to_string())?;
    if !directory.is_dir() {
        return Err("权重路径不是目录，保留原资源".into());
    }
    let artifact = std::fs::metadata(path.join(artifact))
        .map_err(|_| "目录里没有该卡的权重文件，保留原资源".to_owned())?;
    if !artifact.is_file() {
        return Err("卡片权重不是普通文件，保留原资源".into());
    }
    Ok(Some(Identity {
        canonical,
        directory: (directory.dev(), directory.ino()),
        entry: (entry.dev(), entry.ino()),
        artifact: (artifact.dev(), artifact.ino()),
        linked_entry: entry.file_type().is_symlink(),
    }))
}

fn checked_home(value: Option<&std::ffi::OsStr>) -> Result<PathBuf, String> {
    let home = value
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
        .filter(|path| path.is_absolute())
        .ok_or("用户主目录未配置为绝对路径，保留资源以便核实")?;
    let actual = home
        .canonicalize()
        .map_err(|_| "不能核实用户主目录，保留原资源".to_owned())?;
    if !actual.is_dir() {
        return Err("用户主目录不是目录，保留原资源".into());
    }
    Ok(home)
}

impl Target {
    pub(super) fn capture(app: &App, card: &str, path: PathBuf) -> Result<Self, String> {
        let declaration = find_card_raw(app, card).map_err(|e| e.2)?;
        let deploy = declaration.deploy.ok_or("卡片缺部署信息，保留权重")?;
        if Path::new(&deploy.model_dir) != path {
            return Err("权重目录在预检期间变化，保留原资源".into());
        }
        let home = checked_home(std::env::var_os("HOME").as_deref())?;
        let identity = observe(&path, &deploy.artifact, &home)?;
        Ok(Self {
            path,
            artifact: deploy.artifact,
            home,
            identity,
        })
    }

    pub(super) fn verify(&self, app: &App, card: &str) -> Result<(), String> {
        let declaration = find_card_raw(app, card).map_err(|e| e.2)?;
        let deploy = declaration.deploy.ok_or("卡片部署信息消失，保留权重")?;
        if deploy.artifact != self.artifact
            || (self.identity.is_none() && Path::new(&deploy.model_dir) != self.path)
            || observe(&self.path, &self.artifact, &self.home)? != self.identity
            || observe(Path::new(&deploy.model_dir), &deploy.artifact, &self.home)? != self.identity
        {
            return Err("权重目录或部署归属在停止期间变化，保留资源与登记以便核实".into());
        }
        Ok(())
    }

    pub(super) fn path(&self) -> &Path {
        &self.path
    }

    pub(super) fn exists(&self) -> bool {
        self.identity.is_some()
    }

    pub(super) fn is_link(&self) -> bool {
        self.identity.as_ref().is_some_and(|id| id.linked_entry)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::docker::fixtures as fx;

    #[test]
    fn missing_empty_relative_or_unverifiable_home_is_never_guessed() {
        let root = fx::tmpdir("purge-home-validation");
        for value in [
            None,
            Some(std::ffi::OsStr::new("")),
            Some(std::ffi::OsStr::new("relative/home")),
            Some(root.join("absent").as_os_str()),
        ] {
            assert!(checked_home(value).is_err());
        }
        let file = root.join("not-a-directory");
        std::fs::write(&file, b"preserve").unwrap();
        assert!(checked_home(Some(file.as_os_str())).is_err());
        assert_eq!(checked_home(Some(root.as_os_str())).unwrap(), root);
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn root_home_ancestors_and_aliases_are_never_destructive_targets() {
        let root = fx::tmpdir("purge-target-dangerous");
        let home = root.join("home");
        std::fs::create_dir_all(home.join("child")).unwrap();
        std::fs::write(home.join("model.tachyon"), b"preserve").unwrap();
        let alias = root.join("home-alias");
        std::os::unix::fs::symlink(&home, &alias).unwrap();
        for target in [
            PathBuf::from("/"),
            root.clone(),
            home.clone(),
            PathBuf::from(format!("{}/", home.display())),
            home.join("."),
            home.join("child/.."),
            alias.clone(),
            alias.join("child/.."),
        ] {
            for declared_home in [&home, &alias] {
                assert!(
                    observe(&target, "model.tachyon", declared_home).is_err(),
                    "{target:?}"
                );
            }
        }
        assert_eq!(
            std::fs::read(home.join("model.tachyon")).unwrap(),
            b"preserve"
        );
        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn directory_replacement_and_missing_to_present_change_identity_without_following_links() {
        let root = fx::tmpdir("purge-target-identity");
        let home = root.join("home");
        let weights = root.join("weights");
        std::fs::create_dir(&home).unwrap();
        let missing = observe(&weights, "model.tachyon", &home).unwrap();
        assert!(missing.is_none());
        std::fs::create_dir(&weights).unwrap();
        std::fs::write(weights.join("model.tachyon"), b"old").unwrap();
        let before = observe(&weights, "model.tachyon", &home).unwrap();
        assert_ne!(before, missing);
        std::fs::rename(&weights, root.join("original")).unwrap();
        std::fs::create_dir(&weights).unwrap();
        std::fs::write(weights.join("model.tachyon"), b"new").unwrap();
        assert_ne!(before, observe(&weights, "model.tachyon", &home).unwrap());
        let alias = root.join("alias");
        std::os::unix::fs::symlink(&weights, &alias).unwrap();
        let observed = observe(&alias, "model.tachyon", &home).unwrap().unwrap();
        assert!(observed.linked_entry);
        // The execution path is the original alias, not the resolved target.
        std::fs::remove_dir_all(&alias).unwrap();
        assert_eq!(
            std::fs::read(weights.join("model.tachyon")).unwrap(),
            b"new"
        );
        std::fs::remove_dir_all(root).unwrap();
    }
}
