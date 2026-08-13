import React, { useMemo } from 'react';
import { BrainCircuit, Code, FileText, FolderOpen, Package } from '../../components/icons.jsx';
import { projectMaterialPackages } from './team-relay-state.js';

function displayTime(value) {
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? String(value || '') : date.toLocaleString();
}

export function MaterialInspector({
  copy,
  task,
  taskWorkspace,
  workspaceLoading,
  onPrepare,
  onDownload,
  onOpenPreparedMaterial,
  onRevealPreparedMaterial,
  onLocatePackage,
}) {
  const packages = useMemo(
    () => projectMaterialPackages(task, taskWorkspace),
    [task, taskWorkspace],
  );
  const fileCount = packages.reduce((count, item) => count + item.files.length, 0);
  const preparedCount = packages.reduce(
    (count, item) => count + item.files.filter(itemFile => itemFile.prepared).length,
    0,
  );

  return <aside className="team-relay-material-inspector" aria-label={copy.materialInspector.title} data-testid="team-relay-material-inspector">
    <header>
      <div>
        <span><FolderOpen size={17} /><strong>{copy.materialInspector.title}</strong></span>
        <small>{copy.materialInspector.summary(fileCount, packages.length)}{preparedCount ? ` · ${copy.preparedMaterials(preparedCount, fileCount)}` : ''}</small>
      </div>
      {!!fileCount && <button type="button" disabled={workspaceLoading || preparedCount === fileCount} onClick={onPrepare}>
        {workspaceLoading ? copy.preparingMaterials : copy.prepareAllMaterials}
      </button>}
    </header>
    <div className="team-relay-material-inspector-scroll">
      {!packages.length && <div className="team-relay-material-inspector-empty"><FolderOpen size={26} /><p>{copy.materialInspector.empty}</p></div>}
      {packages.map(item => <section className="team-relay-material-inspector-package" key={item.id}>
        <button type="button" className="team-relay-material-inspector-package-title" onClick={() => onLocatePackage(item.id)}>
          <span><Package size={13} />{copy.packageId(item.sequence)} · {copy.artifactRoles[item.delivery.artifact_role]}</span>
          <time>{displayTime(item.time)}</time>
        </button>
        <p>{item.actor} · {item.kindLabel || copy.actionKinds[item.kind] || item.kind}</p>
        {item.agentContextUpdated && <div className="team-relay-material-inspector-update"><BrainCircuit size={12} />{copy.materialInspector.agentUpdate}</div>}
        {item.files.map(itemFile => {
          const { file, prepared, introduced } = itemFile;
          const unavailable = file.available === false;
          return <div className={`team-relay-material-inspector-file${prepared ? ' is-prepared' : ''}${unavailable ? ' is-unavailable' : ''}`} key={file.id}>
            <FileText size={16} />
            <span><strong>{file.original_name}</strong>{unavailable && <small>{copy.materialInspector.unavailable}</small>}</span>
            {introduced && <b>{copy.materialInspector.introduced}</b>}
            {!unavailable && <div className="team-relay-material-inspector-file-actions">
              <button type="button" onClick={() => prepared ? onOpenPreparedMaterial(prepared.local_path) : onDownload(file)}>{prepared ? copy.materialInspector.openPrepared : copy.materialInspector.download}</button>
              {prepared && <button type="button" onClick={() => onRevealPreparedMaterial(file.id)}>{copy.materialInspector.revealPrepared}</button>}
            </div>}
          </div>;
        })}
        {item.delivery.git_references.map((reference, index) => <div className="team-relay-material-inspector-git" key={`${item.id}-inspector-git-${index}`}>
          <Code size={14} />
          <span><code>{reference.repository_url}</code><small>{reference.commit_sha ? `@ ${reference.commit_sha.slice(0, 12)}` : copy.mutableGitWarning}{reference.branch ? ` · ${reference.branch}` : ''}</small></span>
        </div>)}
      </section>)}
    </div>
  </aside>;
}
