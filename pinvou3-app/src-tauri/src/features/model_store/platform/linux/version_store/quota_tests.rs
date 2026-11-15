use super::quota_dir_size_bytes;
use std::fs;
use std::os::unix::fs::{MetadataExt, PermissionsExt, symlink};

#[test]
fn quota_scan_counts_files_without_following_any_links() {
    let temp = tempfile::tempdir().unwrap();
    let root = temp.path().join("candidate");
    fs::create_dir_all(root.join("nested")).unwrap();
    fs::write(root.join("nested/weight"), b"weights").unwrap();
    let outside = temp.path().join("outside");
    fs::create_dir(&outside).unwrap();
    fs::write(outside.join("unrelated"), vec![0; 65536]).unwrap();
    symlink(&outside, root.join("linked-directory")).unwrap();
    symlink(outside.join("unrelated"), root.join("linked-file")).unwrap();
    symlink(root.join("missing"), root.join("dangling-link")).unwrap();
    assert_eq!(quota_dir_size_bytes(&root).unwrap(), 7);
    let linked_root = temp.path().join("linked-root");
    symlink(&outside, &linked_root).unwrap();
    assert!(quota_dir_size_bytes(&linked_root).is_err());
    let dangling_root = temp.path().join("dangling-root");
    symlink(temp.path().join("missing"), &dangling_root).unwrap();
    assert!(quota_dir_size_bytes(&dangling_root).is_err());
    assert_eq!(
        quota_dir_size_bytes(&temp.path().join("absent")).unwrap(),
        0
    );
}

#[test]
fn quota_scan_requires_positive_absence_instead_of_permission_errors() {
    assert_ne!(fs::metadata("/proc/self").unwrap().uid(), 0);
    let temp = tempfile::tempdir().unwrap();
    let parent = temp.path().join("parent");
    let root = parent.join("candidate");
    fs::create_dir_all(&root).unwrap();
    fs::write(root.join("partial"), vec![0; 65536]).unwrap();
    fs::set_permissions(&parent, fs::Permissions::from_mode(0o000)).unwrap();
    let present = quota_dir_size_bytes(&root);
    let unresolved_absence = quota_dir_size_bytes(&parent.join("missing"));
    fs::set_permissions(&parent, fs::Permissions::from_mode(0o700)).unwrap();
    assert!(present.is_err());
    assert!(unresolved_absence.is_err());
    assert_eq!(quota_dir_size_bytes(&root).unwrap(), 65536);
    fs::remove_dir_all(&root).unwrap();
    assert_eq!(quota_dir_size_bytes(&root).unwrap(), 0);
}

#[test]
fn quota_scan_rejects_linked_ancestors_even_when_the_leaf_is_absent() {
    let temp = tempfile::tempdir().unwrap();
    let real = temp.path().join("real");
    std::fs::create_dir_all(real.join("leaf")).unwrap();
    std::fs::write(real.join("leaf/weight"), b"weight").unwrap();
    let link = temp.path().join("linked");
    std::os::unix::fs::symlink(&real, &link).unwrap();
    assert!(quota_dir_size_bytes(&link.join("leaf")).is_err());
    assert!(quota_dir_size_bytes(&link.join("missing")).is_err());
    assert_eq!(quota_dir_size_bytes(&real.join("leaf")).unwrap(), 6);
}
