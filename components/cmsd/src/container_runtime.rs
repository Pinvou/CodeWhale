//! Read-only proof shared by container control and gateway authorization.
//! An equivalent recreated deployment may be managed; a colliding reference is
//! not permission to stop, reconfigure or route another workload.
use crate::app::App;
use crate::cards::{Card, EngineKind};
use crate::docker::ContainerInfo;
use crate::store::ModelState;
mod network;

pub fn saved_reference(model: &ModelState, container: &ContainerInfo) -> Result<(), String> {
    if model.container_name.as_deref() != Some(container.name.as_str())
        || model.image.as_deref().filter(|image| !image.is_empty())
            != Some(container.image.as_str())
    {
        return Err(
            "configuration_changed: 实际容器与已保存托管引用不一致，保留外来对象及策略".into(),
        );
    }
    Ok(())
}

/// Callers may pass the last owned reference. Current card edits cannot redirect
/// that existing grant to a different container or image.
pub fn check(
    card: &Card,
    model: Option<&ModelState>,
    container: &ContainerInfo,
) -> Result<(), String> {
    let deploy = card
        .docker_deploy
        .as_ref()
        .ok_or("not_deployed: 缺少容器部署记录")?;
    if card.engine_kind != EngineKind::Container
        || card.container_name.as_deref() != Some(deploy.container_name.as_str())
        || container.name != deploy.container_name
    {
        return Err("configuration_changed: 卡片与部署容器引用不一致".into());
    }
    if let Some(model) = model.filter(|model| model.lifecycle_owned) {
        if model.engine_type != "container" {
            return Err("configuration_changed: 托管运行时类型变化".into());
        }
        saved_reference(model, container)?;
    }
    crate::ops::validate_container_config(deploy, container)?;
    network::check(deploy, container)?;
    if !container.model_mount_matches(&deploy.model_dir)? {
        return Err("configuration_changed: 容器未挂载当前部署的权重目录，保留原实例及策略".into());
    }
    Ok(())
}

pub fn inspect(
    app: &App,
    card: &Card,
    model: Option<&ModelState>,
) -> Result<Option<ContainerInfo>, String> {
    let deploy = card
        .docker_deploy
        .as_ref()
        .ok_or("not_deployed: 缺少容器部署记录")?;
    if let Some(model) = model.filter(|model| model.lifecycle_owned) {
        if model
            .container_name
            .as_deref()
            .is_some_and(|name| name != deploy.container_name)
            || model
                .image
                .as_deref()
                .is_some_and(|image| image != deploy.image)
        {
            return Err("configuration_changed: 当前卡片与原托管容器引用不一致".into());
        }
    }
    if card.engine_kind != EngineKind::Container
        || card.container_name.as_deref() != Some(deploy.container_name.as_str())
    {
        return Err("configuration_changed: 卡片与部署容器引用不一致".into());
    }
    let container = app.docker.inspect(&deploy.container_name)?;
    if let Some(container) = &container {
        check(card, model, container)?;
    }
    Ok(container)
}

pub fn inspect_recorded(app: &App, model: &ModelState) -> Result<Option<ContainerInfo>, String> {
    let card = crate::ops::find_card_raw(app, &model.card).map_err(|error| error.2)?;
    inspect(app, &card, Some(model))
}

pub fn routable(app: &App, model: &ModelState) -> bool {
    if !model.lifecycle_owned || model.engine_type != "container" || model.port == 0 {
        return false;
    }
    let Ok(card) = crate::ops::find_card_raw(app, &model.card) else {
        return false;
    };
    if card
        .docker_deploy
        .as_ref()
        .is_none_or(|deploy| deploy.port != model.port)
    {
        return false;
    }
    inspect(app, &card, Some(model)).is_ok_and(|container| {
        container.is_some_and(|container| container.running && container.pid > 0)
    })
}

#[cfg(test)]
mod tests;
