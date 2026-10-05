export function buildUnitTree(units) {
  const nodes = new Map((units || []).map(unit => [unit.unit_id, { ...unit, children:[] }]));
  const roots = [];
  for (const node of nodes.values()) {
    const parent = node.parent_id && nodes.get(node.parent_id);
    if (parent && parent !== node) parent.children.push(node);
    else roots.push(node);
  }
  return roots;
}
