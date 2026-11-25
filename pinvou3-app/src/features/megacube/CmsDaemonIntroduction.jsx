import React from 'react';

export function CmsDaemonIntroduction({ copy, expanded }) {
  return <div className="mt-3 text-sm" data-testid="cms-service-introduction">
    <details key={expanded ? 'install' : 'installed'} open={expanded} className="cms-details cms-service-about">
      <summary>{expanded ? copy.benefitsTitle : copy.aboutService}</summary>
      <p className="cms-muted text-sm">{copy.introduction}</p>
      <ul className="mt-3 grid list-disc gap-x-6 gap-y-2 pl-5 sm:grid-cols-2">
        {copy.benefits.map(({ title, description }) => <li key={title}><span className="font-medium">{title}</span><p className="mt-1 text-xs text-black/60 dark:text-white/60">{description}</p></li>)}
      </ul>
      <p className="mt-3 text-xs text-black/60 dark:text-white/60">{copy.installBoundary}</p>
    </details>
  </div>;
}
