//! App integration tests use the same revision storage and reuse predicate as
//! EnginePool without constructing a Wry app, tools or an inference engine.

use super::{ModelUpdateRevisions, PreparedRuntimeModel, PreparedRuntimeState, SavedModel};
use std::sync::Arc;

pub(crate) struct RevisionProbe {
    revisions: ModelUpdateRevisions,
}

#[derive(Clone)]
pub(crate) struct CachedState(PreparedRuntimeState);

impl RevisionProbe {
    pub(crate) fn new() -> Self {
        Self {
            revisions: ModelUpdateRevisions::default(),
        }
    }

    pub(crate) fn marker(&self) -> Arc<dyn Fn(&[String]) + Send + Sync> {
        let revisions = self.revisions.clone();
        Arc::new(move |ids| {
            for id in ids {
                revisions.bump(id);
            }
        })
    }

    pub(crate) fn snapshot(&self, model: SavedModel) -> CachedState {
        let revision = self.revisions.current(&model.id);
        CachedState(PreparedRuntimeState::new(
            PreparedRuntimeModel::unchanged(model),
            false,
            revision,
        ))
    }
}

impl CachedState {
    pub(crate) fn requires_rebuild_from(&self, before: &Self) -> bool {
        self.0.requires_rebuild_from(&before.0)
    }
}
