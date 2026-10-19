-- 000058 permits multiple components of one type and removes the former
-- package-leading unique index. Keep package reads and expiry locks indexed
-- without restoring that obsolete uniqueness or excluding terminal components.
CREATE INDEX entitlement_components_package_idx
    ON public.entitlement_components(package_id);
