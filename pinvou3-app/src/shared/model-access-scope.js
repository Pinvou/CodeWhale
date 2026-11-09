export function modelAccessAppliesToView(bs, currentView, bridgeAvailable, isRemoteTranscriptActive = false) {
  return !!(bridgeAvailable && !isRemoteTranscriptActive
    && (currentView === 'chat' || currentView === 'scheduled' && bs?.scheduledRunContext));
}
