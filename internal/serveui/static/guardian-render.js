(() => {
'use strict';

const app = window.TermLLMApp;
const createEl = app.createEl;

const reviewText = (review) => String(review?.message || '').trim().replace(/^guardian:\s*/i, '');

const buildReviewsNode = (tool) => {
  const reviews = Array.isArray(tool?.guardianReviews) ? tool.guardianReviews : [];
  if (reviews.length === 0) return null;
  const body = createEl('div', 'tool-entry-guardian');
  reviews.forEach((review) => {
    const outcome = String(review?.outcome || 'warning').trim().toLowerCase();
    const row = createEl('div', `tool-guardian-review ${outcome}`);
    row.appendChild(createEl('span', 'tool-guardian-icon', '🛡'));
    row.appendChild(createEl('span', 'tool-guardian-text', `Guardian: ${reviewText(review)}`));
    body.appendChild(row);
  });
  return body;
};

const appendReviews = (parent, tool) => {
  const node = buildReviewsNode(tool);
  if (parent && node) parent.appendChild(node);
};

const syncReviews = (parent, tool) => {
  if (!parent) return;
  const existing = parent.querySelector('.tool-entry-guardian');
  const next = buildReviewsNode(tool);
  if (existing) existing.remove();
  if (!next) return;
  const subagent = parent.querySelector('.subagent-result');
  if (subagent) parent.insertBefore(next, subagent);
  else parent.appendChild(next);
};

const summaryInfo = (tools) => {
  const reviews = (Array.isArray(tools) ? tools : []).flatMap((tool) => Array.isArray(tool.guardianReviews) ? tool.guardianReviews : []);
  const counts = { approved: 0, denied: 0, warning: 0 };
  reviews.forEach((review) => {
    const outcome = String(review?.outcome || 'warning').toLowerCase();
    if (outcome === 'approved') counts.approved += 1;
    else if (outcome === 'denied' || outcome === 'error') counts.denied += 1;
    else counts.warning += 1;
  });
  const parts = [counts.approved && `${counts.approved} approved`, counts.denied && `${counts.denied} denied`, counts.warning && `${counts.warning} warning`].filter(Boolean);
  return { text: parts.length ? `🛡 ${parts.join(' · ')}` : '', tone: counts.denied ? 'denied' : (counts.warning ? 'warning' : 'approved') };
};

Object.assign(app, {
  appendGuardianReviews: appendReviews,
  syncGuardianReviews: syncReviews,
  guardianSummary: (tools) => summaryInfo(tools).text,
  guardianSummaryTone: (tools) => summaryInfo(tools).tone,
});
})();
