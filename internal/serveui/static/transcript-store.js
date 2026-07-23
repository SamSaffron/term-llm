(function initTranscriptStore(root, factory) {
  const api = factory(root || {});
  if (typeof module === 'object' && module.exports) module.exports = api;
  if (root) Object.assign(root, api);
})(typeof globalThis !== 'undefined' ? globalThis : this, (root) => {
  'use strict';

  const TRANSCRIPT_FLAG_COMPACTION_TAIL = 1;
  const TRANSCRIPT_FLAG_EMPTY_BODY = 2;
  const TRANSCRIPT_MATERIALIZE_BATCH_TURNS = 32;
  const DEFAULT_TRANSCRIPT_BUDGETS = Object.freeze({
    maxMaterializedTurns: 60,
    overscanTurns: 8,
    maxRecentSkeletons: 2
  });
  const TRANSCRIPT_BUDGETS = Object.freeze(Object.assign(
    {},
    DEFAULT_TRANSCRIPT_BUDGETS,
    root.TERM_LLM_TRANSCRIPT_BUDGETS || {}
  ));
  const storesBySession = new Map();

  const roleCode = (role) => {
    switch (String(role || '')) {
      case 'user': return 'u';
      case 'assistant': return 'a';
      case 'tool': return 't';
      case 'event': return 'e';
      default: return '?';
    }
  };
  const roleName = (code) => ({ u: 'user', a: 'assistant', t: 'tool', e: 'event' }[code] || 'event');
  const finiteInt = (value, fallback = 0) => Number.isFinite(Number(value)) ? Math.trunc(Number(value)) : fallback;
  const normalizedID = (value) => Number.isFinite(Number(value)) ? Number(value) : value;
  const bodyID = (entry) => normalizedID(entry?.id ?? entry?.ID);
  const bodySeq = (entry) => finiteInt(entry?.sequence ?? entry?.seq, -1);
  const partToolID = (part) => String(part?.tool_call_id || part?.call_id || part?.toolCallId || part?.id || '').trim();

  class TranscriptStore {
    constructor(sessionId, budgets = {}) {
      this.sessionId = String(sessionId || '');
      this.budgets = Object.assign({}, TRANSCRIPT_BUDGETS, budgets || {});
      this.budgets.maxMaterializedTurns = Math.max(1, finiteInt(this.budgets.maxMaterializedTurns, 60));
      this.budgets.overscanTurns = Math.max(0, finiteInt(this.budgets.overscanTurns, 8));
      this.rev = 0;
      this.ids = [];
      this.seqs = [];
      this.roles = '';
      this.flags = [];
      this.compactionSeq = -1;
      this.compactionCount = 0;
      this.bodies = new Map();
      this.segments = [];
      this.optimistic = [];
      this.persistedOptimistic = new WeakSet();
      this.activeRun = null;
      this.etag = '';
      this.viewport = { firstOrdinal: -1, lastOrdinal: -1 };
      this.pinnedSegments = new Set();
      this.stats = { indexFetches: 0, bodyFetches: 0, rewrites: 0, evictions: 0 };
      storesBySession.set(this.sessionId, this);
    }

    noteIndexFetch(notModified = false, etag = '') {
      this.stats.indexFetches += 1;
      if (etag) this.etag = String(etag);
      return Boolean(notModified);
    }

    applyIndex(envelope, etag = '') {
      const rows = envelope?.rows || {};
      const ids = Array.isArray(rows.ids) ? rows.ids.map(normalizedID) : [];
      const seqs = Array.isArray(rows.seqs) ? rows.seqs.map((seq) => finiteInt(seq, -1)) : [];
      const flags = Array.isArray(rows.flags) ? rows.flags.map((flag) => finiteInt(flag, 0)) : [];
      const roles = String(rows.roles || '');
      const incomingRev = finiteInt(envelope?.rev, this.rev);
      const rollback = incomingRev < this.rev;
      if (ids.length !== seqs.length || ids.length !== flags.length || ids.length !== roles.length) {
        throw new Error('invalid transcript index parallel arrays');
      }
      const duplicateCheck = new Set(ids);
      if (duplicateCheck.size !== ids.length) throw new Error('duplicate durable transcript row ID');

      let divergence = 0;
      const common = Math.min(this.ids.length, ids.length);
      while (divergence < common
        && this.ids[divergence] === ids[divergence]
        && this.seqs[divergence] === seqs[divergence]
        && this.roles[divergence] === roles[divergence]
        && this.flags[divergence] === flags[divergence]) {
        divergence += 1;
      }
      const compactionChanged = this.compactionSeq !== finiteInt(envelope?.compaction_seq, -1)
        || this.compactionCount !== finiteInt(envelope?.compaction_count, 0);
      const changed = rollback || divergence !== this.ids.length || divergence !== ids.length || compactionChanged;
      const appendOnly = !rollback && !compactionChanged && divergence === this.ids.length && ids.length >= this.ids.length;
      if (changed && !appendOnly) this.stats.rewrites += 1;

      if (rollback) {
        this.bodies.clear();
        for (const local of this.optimistic) local.revAtSend = incomingRev;
      }
      const surviving = new Set(ids);
      for (const id of this.bodies.keys()) {
        if (!surviving.has(id)) this.bodies.delete(id);
      }
      const oldSegmentState = this.segmentStateByDurableRange();
      this.ids = ids;
      this.seqs = seqs;
      this.roles = roles;
      this.flags = flags;
      this.compactionSeq = finiteInt(envelope?.compaction_seq, -1);
      this.compactionCount = finiteInt(envelope?.compaction_count, 0);
      this.rebuildSegments(oldSegmentState);
      this.rev = incomingRev;
      if (etag) this.etag = String(etag);
      this.refreshPinnedSegments();
      this.enforceBudget();
      return { changed, appendOnly, divergence, rollback };
    }

    segmentStateByDurableRange() {
      const result = new Map();
      for (const segment of this.segments) {
        const firstID = this.ids[segment.startOrdinal];
        const lastID = this.ids[segment.endOrdinal];
        result.set(`${firstID}:${lastID}`, { state: segment.state, estHeight: segment.estHeight });
      }
      return result;
    }

    rebuildSegments(oldState = new Map()) {
      const segments = [];
      if (this.ids.length === 0) {
        this.segments = segments;
        return;
      }
      let start = 0;
      for (let ordinal = 1; ordinal < this.ids.length; ordinal += 1) {
        if (this.roles[ordinal] === 'u') {
          segments.push(this.makeSegment(start, ordinal - 1, oldState));
          start = ordinal;
        }
      }
      segments.push(this.makeSegment(start, this.ids.length - 1, oldState));
      this.segments = segments;
      for (const segment of this.segments) this.refreshSegmentState(segment);
    }

    makeSegment(startOrdinal, endOrdinal, oldState) {
      const key = `${this.ids[startOrdinal]}:${this.ids[endOrdinal]}`;
      const previous = oldState.get(key);
      return {
        startOrdinal,
        endOrdinal,
        state: previous?.state || 'evicted',
        estHeight: Math.max(1, finiteInt(previous?.estHeight, this.defaultSegmentHeight(startOrdinal, endOrdinal)))
      };
    }

    defaultSegmentHeight(startOrdinal, endOrdinal) {
      return 44 + Math.max(0, endOrdinal - startOrdinal) * 28;
    }

    segmentForOrdinal(ordinal) {
      if (ordinal < 0 || ordinal >= this.ids.length) return -1;
      let low = 0;
      let high = this.segments.length - 1;
      while (low <= high) {
        const mid = (low + high) >> 1;
        const segment = this.segments[mid];
        if (ordinal < segment.startOrdinal) high = mid - 1;
        else if (ordinal > segment.endOrdinal) low = mid + 1;
        else return mid;
      }
      return -1;
    }

    ordinalForID(id) {
      return this.ids.indexOf(normalizedID(id));
    }

    segmentForID(id) {
      return this.segmentForOrdinal(this.ordinalForID(id));
    }

    requiredBodyIDs(segmentIndex) {
      const segment = this.segments[segmentIndex];
      if (!segment) return [];
      const ids = [];
      for (let ordinal = segment.startOrdinal; ordinal <= segment.endOrdinal; ordinal += 1) {
        if ((this.flags[ordinal] & TRANSCRIPT_FLAG_EMPTY_BODY) === 0) ids.push(this.ids[ordinal]);
      }
      return ids;
    }

    refreshSegmentState(segment) {
      const index = this.segments.indexOf(segment);
      const required = this.requiredBodyIDs(index);
      segment.state = required.length === 0
        ? 'empty'
        : (required.every((id) => this.bodies.has(id)) ? 'materialized' : 'evicted');
      if (segment.state === 'evicted') {
        for (let ordinal = segment.startOrdinal; ordinal <= segment.endOrdinal; ordinal += 1) {
          this.bodies.delete(this.ids[ordinal]);
        }
      }
      return segment.state;
    }

    materialize(messages, options = {}) {
      if (!Array.isArray(messages)) return [];
      this.stats.bodyFetches += options.countFetch === false ? 0 : 1;
      const known = new Set(this.ids);
      const touched = new Set();
      for (const entry of messages) {
        const id = bodyID(entry);
        if (!known.has(id)) continue;
        this.bodies.set(id, entry);
        const segmentIndex = this.segmentForID(id);
        if (segmentIndex >= 0) touched.add(segmentIndex);
      }
      for (const segmentIndex of touched) this.refreshSegmentState(this.segments[segmentIndex]);
      if (!options.deferBudget) this.enforceBudget();
      return [...touched];
    }

    materializeSegment(segmentIndex, messages, estHeight = null) {
      const segment = this.segments[segmentIndex];
      if (!segment) return false;
      this.materialize(messages);
      this.refreshSegmentState(segment);
      if (estHeight != null) segment.estHeight = Math.max(1, finiteInt(estHeight, segment.estHeight));
      return segment.state === 'materialized';
    }

    evictSegment(segmentIndex, estHeight = null) {
      const segment = this.segments[segmentIndex];
      if (!segment || segment.state !== 'materialized' || this.pinnedSegments.has(segmentIndex)) return false;
      if (estHeight != null) segment.estHeight = Math.max(1, finiteInt(estHeight, segment.estHeight));
      for (let ordinal = segment.startOrdinal; ordinal <= segment.endOrdinal; ordinal += 1) {
        this.bodies.delete(this.ids[ordinal]);
      }
      segment.state = 'evicted';
      this.stats.evictions += 1;
      return true;
    }

    setViewport(firstOrdinal, lastOrdinal, options = {}) {
      this.viewport = {
        firstOrdinal: Math.max(-1, finiteInt(firstOrdinal, -1)),
        lastOrdinal: Math.max(-1, finiteInt(lastOrdinal, -1))
      };
      this.refreshPinnedSegments();
      if (!options.deferBudget) this.enforceBudget();
    }

    refreshPinnedSegments() {
      const pinned = new Set();
      if (this.segments.length === 0) {
        this.pinnedSegments = pinned;
        return pinned;
      }
      let first = this.segmentForOrdinal(this.viewport.firstOrdinal);
      let last = this.segmentForOrdinal(this.viewport.lastOrdinal);
      if (first < 0 && this.viewport.firstOrdinal < 0) {
        first = Math.max(0, this.segments.length - 1 - this.budgets.overscanTurns);
        last = this.segments.length - 1;
      }
      if (first >= 0) {
        if (last < first) last = first;
        const from = Math.max(0, first - this.budgets.overscanTurns);
        const to = Math.min(this.segments.length - 1, last + this.budgets.overscanTurns);
        for (let i = from; i <= to; i += 1) pinned.add(i);
      }
      this.pinnedSegments = pinned;
      return pinned;
    }

    enforceBudget() {
      const materialized = [];
      for (let i = 0; i < this.segments.length; i += 1) {
        if (this.segments[i].state === 'materialized') materialized.push(i);
      }
      // This is deliberately a turn/segment budget, never a durable-row or byte
      // budget. A visible pinned turn may contain arbitrarily many tool rows and
      // can exceed practical memory/DOM byte targets; preserving complete visible
      // content and conversion context wins. Unpinned distant turns are still
      // evicted so the rest of the transcript remains bounded.
      const allowed = Math.max(this.budgets.maxMaterializedTurns, [...this.pinnedSegments].filter((i) => this.segments[i]?.state === 'materialized').length);
      if (materialized.length <= allowed) return;
      const viewportCenter = this.viewport.firstOrdinal >= 0
        ? (this.viewport.firstOrdinal + Math.max(this.viewport.firstOrdinal, this.viewport.lastOrdinal)) / 2
        : this.ids.length - 1;
      const candidates = materialized
        .filter((index) => !this.pinnedSegments.has(index))
        .sort((a, b) => {
          const sa = this.segments[a];
          const sb = this.segments[b];
          const da = Math.abs(((sa.startOrdinal + sa.endOrdinal) / 2) - viewportCenter);
          const db = Math.abs(((sb.startOrdinal + sb.endOrdinal) / 2) - viewportCenter);
          return db - da;
        });
      let remaining = materialized.length;
      for (const index of candidates) {
        if (remaining <= allowed) break;
        if (this.evictSegment(index)) remaining -= 1;
      }
    }

    selectGapBatch(startSegmentIndex, endSegmentIndex, options = {}) {
      if (this.segments.length === 0) return [];
      const requestedStart = finiteInt(startSegmentIndex, -1);
      const requestedEnd = finiteInt(endSegmentIndex, -1);
      if (requestedStart < 0 || requestedEnd < requestedStart) return [];
      const start = Math.min(this.segments.length - 1, requestedStart);
      const end = Math.max(start, Math.min(this.segments.length - 1, requestedEnd));

      const targetOrdinal = finiteInt(options.targetOrdinal, this.segments[start].startOrdinal);
      const target = Math.max(start, Math.min(end, this.segmentForOrdinal(targetOrdinal)));
      const direction = String(options.direction || 'center');
      const selected = [];
      const consider = (index) => {
        const segment = this.segments[index];
        if (!segment || segment.state !== 'evicted') return true;
        if (selected.length >= TRANSCRIPT_MATERIALIZE_BATCH_TURNS) return false;
        // Each selected segment is represented on the wire by one turn anchor.
        // Expanded durable row count is intentionally irrelevant: the server
        // returns the complete user-bounded turn and the UI never splits it.
        selected.push(index);
        return selected.length < TRANSCRIPT_MATERIALIZE_BATCH_TURNS;
      };

      if (direction === 'backward') {
        for (let index = target; index >= start && consider(index); index -= 1) {}
      } else if (direction === 'forward') {
        for (let index = target; index <= end && consider(index); index += 1) {}
      } else {
        if (consider(target)) {
          for (let distance = 1; selected.length < TRANSCRIPT_MATERIALIZE_BATCH_TURNS; distance += 1) {
            const before = target - distance;
            const after = target + distance;
            if (before < start && after > end) break;
            if (before >= start && !consider(before)) break;
            if (after <= end && !consider(after)) break;
          }
        }
      }
      return selected.sort((a, b) => a - b);
    }

    renderRuns() {
      const runs = [];
      for (let index = 0; index < this.segments.length; index += 1) {
        const segment = this.segments[index];
        if (segment.state === 'empty') {
          const previous = runs[runs.length - 1];
          if (previous?.type === 'gap') {
            previous.endOrdinal = segment.endOrdinal;
            previous.endSegmentIndex = index;
          }
          continue;
        }
        if (segment.state === 'materialized') {
          runs.push({
            type: 'segment',
            segmentIndex: index,
            startOrdinal: segment.startOrdinal,
            endOrdinal: segment.endOrdinal,
            ids: this.ids.slice(segment.startOrdinal, segment.endOrdinal + 1)
          });
          continue;
        }
        const previous = runs[runs.length - 1];
        if (previous?.type === 'gap' && previous.endOrdinal + 1 === segment.startOrdinal) {
          previous.endOrdinal = segment.endOrdinal;
          previous.endSegmentIndex = index;
          previous.height += segment.estHeight;
        } else {
          runs.push({
            type: 'gap',
            startOrdinal: segment.startOrdinal,
            endOrdinal: segment.endOrdinal,
            startSegmentIndex: index,
            endSegmentIndex: index,
            height: segment.estHeight
          });
        }
      }
      return runs;
    }

    renderedMessages() {
      const result = [];
      for (const run of this.renderRuns()) {
        if (run.type === 'gap') {
          result.push({
            role: 'transcript-gap',
            transcriptGap: true,
            startOrdinal: run.startOrdinal,
            endOrdinal: run.endOrdinal,
            startSegmentIndex: run.startSegmentIndex,
            endSegmentIndex: run.endSegmentIndex,
            estimatedHeight: run.height
          });
          continue;
        }
        for (let ordinal = run.startOrdinal; ordinal <= run.endOrdinal; ordinal += 1) {
          if ((this.flags[ordinal] & TRANSCRIPT_FLAG_EMPTY_BODY) !== 0) continue;
          const body = this.bodies.get(this.ids[ordinal]);
          if (body) result.push(body);
        }
      }
      result.push(...this.optimistic);
      return result;
    }

    addOptimistic(entry, revAtSend = this.rev, options = {}) {
      if (!entry || typeof entry !== 'object') return null;
      const clientKey = String(entry.clientKey || entry.id || `optimistic-${Date.now()}-${this.optimistic.length}`);
      const existing = this.optimistic.find((item) => String(item.clientKey || item.id || '') === clientKey);
      if (existing) {
        if (options.persisted === true) this.persistedOptimistic.add(existing);
        return existing;
      }
      const optimistic = entry;
      optimistic.clientKey = clientKey;
      optimistic.optimistic = true;
      optimistic.revAtSend = finiteInt(entry.revAtSend, finiteInt(revAtSend, this.rev));
      optimistic.durableSeqAtSend = finiteInt(entry.durableSeqAtSend, this.seqs.length ? this.seqs[this.seqs.length - 1] : -1);
      this.optimistic.push(optimistic);
      if (options.persisted === true) this.persistedOptimistic.add(optimistic);
      return optimistic;
    }

    setActiveRun(id, startedRev = 0) {
      const normalized = String(id || '').trim();
      this.activeRun = normalized ? { id: normalized, startedRev: finiteInt(startedRev, 0) } : null;
      return this.activeRun;
    }

    segmentAfterSequence(sequence) {
      const boundary = finiteInt(sequence, -1);
      let low = 0;
      let high = this.seqs.length;
      while (low < high) {
        const mid = (low + high) >> 1;
        if (this.seqs[mid] <= boundary) low = mid + 1;
        else high = mid;
      }
      return low < this.seqs.length ? this.segmentForOrdinal(low) : -1;
    }

    persistedOptimisticToolSegmentIndexes(limit = TRANSCRIPT_MATERIALIZE_BATCH_TURNS) {
      if (this.activeRun || this.optimistic.length === 0) return [];
      const max = Math.max(1, finiteInt(limit, TRANSCRIPT_MATERIALIZE_BATCH_TURNS));
      const selected = new Set();
      for (const local of this.optimistic) {
        if (selected.size >= max) break;
        if (!this.persistedOptimistic.has(local)) continue;
        if (local.role !== 'tool-group' && local.role !== 'tool') continue;
        if (this.rev <= finiteInt(local.revAtSend, this.rev)) continue;
        const segmentIndex = this.segmentAfterSequence(local.durableSeqAtSend);
        if (segmentIndex >= 0 && this.segments[segmentIndex]?.state === 'evicted') selected.add(segmentIndex);
      }
      return [...selected];
    }

    durableToolIDsInSegment(segmentIndex, afterSequence = -1) {
      const ids = new Set();
      const segment = this.segments[segmentIndex];
      if (!segment) return ids;
      for (let ordinal = segment.startOrdinal; ordinal <= segment.endOrdinal; ordinal += 1) {
        if (this.seqs[ordinal] <= afterSequence) continue;
        const entry = this.bodies.get(this.ids[ordinal]);
        for (const part of entry?.parts || []) {
          const id = partToolID(part);
          if (id) ids.add(id);
        }
      }
      return ids;
    }

    durableToolIDsAfter(sequence) {
      const ids = new Set();
      for (let ordinal = 0; ordinal < this.ids.length; ordinal += 1) {
        if (this.seqs[ordinal] <= sequence) continue;
        const entry = this.bodies.get(this.ids[ordinal]);
        for (const part of entry?.parts || []) {
          const id = partToolID(part);
          if (id) ids.add(id);
        }
      }
      return ids;
    }

    reconcileOptimistic() {
      if (this.optimistic.length === 0) return [];
      const kept = [];
      const removed = [];
      const consumed = new Set();
      for (const local of this.optimistic) {
        if (this.rev <= finiteInt(local.revAtSend, this.rev)) {
          kept.push(local);
          continue;
        }
        const afterSeq = finiteInt(local.durableSeqAtSend, -1);
        let matched = false;
        if (local.role === 'user') {
          for (let ordinal = 0; ordinal < this.ids.length; ordinal += 1) {
            if (this.seqs[ordinal] <= afterSeq || consumed.has(ordinal)) continue;
            if (this.roles[ordinal] === 'u' && this.bodies.has(this.ids[ordinal])) {
              consumed.add(ordinal);
              matched = true;
              break;
            }
          }
        } else if (local.role === 'tool-group' || local.role === 'tool') {
          const localIDs = new Set();
          for (const tool of local.tools || []) {
            const id = String(tool?.id || tool?.call_id || '').trim();
            if (id) localIDs.add(id);
          }
          for (const part of local.parts || []) {
            const id = partToolID(part);
            if (id) localIDs.add(id);
          }
          const durableIDs = this.persistedOptimistic.has(local)
            ? this.durableToolIDsInSegment(this.segmentAfterSequence(afterSeq), afterSeq)
            : this.durableToolIDsAfter(afterSeq);
          matched = localIDs.size > 0 && [...localIDs].every((id) => durableIDs.has(id));
        } else if (local.role === 'assistant') {
          for (let ordinal = 0; ordinal < this.ids.length; ordinal += 1) {
            if (this.seqs[ordinal] <= afterSeq || consumed.has(ordinal)) continue;
            if (this.roles[ordinal] === 'a' && this.bodies.has(this.ids[ordinal])) {
              consumed.add(ordinal);
              matched = true;
              break;
            }
          }
        }
        // Reaching a newer durable revision with no active run is the terminal
        // authority for completed tool UI. An unmatched completed tool cannot
        // represent queued input and must not remain persisted at the tail.
        const completedTool = (local.role === 'tool-group' || local.role === 'tool')
          && ['done', 'error', 'failed', 'cancelled'].includes(String(local.status || '').toLowerCase());
        if (matched || (!this.activeRun && completedTool)) removed.push(local);
        else kept.push(local);
      }
      this.optimistic = kept;
      return removed;
    }

    clearTransientOptimistic() {
      const removed = this.optimistic.filter((entry) => entry?.transient);
      this.optimistic = this.optimistic.filter((entry) => !entry?.transient);
      return removed;
    }

    withViewportAnchor(adapter, mutation) {
      const anchor = adapter?.capture?.() || null;
      const oldIDs = this.ids.slice();
      const oldOrdinal = anchor ? oldIDs.indexOf(normalizedID(anchor.id)) : -1;
      const finish = (result) => {
        let targetID = anchor ? normalizedID(anchor.id) : null;
        if (anchor && !this.ids.includes(targetID)) {
          targetID = null;
          for (let ordinal = oldOrdinal - 1; ordinal >= 0; ordinal -= 1) {
            if (this.ids.includes(oldIDs[ordinal])) {
              targetID = oldIDs[ordinal];
              break;
            }
          }
          if (targetID == null) {
            for (let ordinal = oldOrdinal + 1; ordinal < oldIDs.length; ordinal += 1) {
              if (this.ids.includes(oldIDs[ordinal])) {
                targetID = oldIDs[ordinal];
                break;
              }
            }
          }
        }
        const targetSegment = this.segmentForID(targetID);
        if (targetSegment >= 0) {
          const targetOrdinal = this.ordinalForID(targetID);
          const span = Math.max(0, this.viewport.lastOrdinal - this.viewport.firstOrdinal);
          this.viewport = {
            firstOrdinal: targetOrdinal,
            lastOrdinal: Math.min(this.ids.length - 1, targetOrdinal + span)
          };
        }
        // Finalize the body budget before projecting the store into the DOM. The
        // previous implementation rendered inside each intermediate mutation,
        // then evicted after rendering and needed another reconciliation pass to
        // make the DOM agree with the store. A transcript transaction now has one
        // observable commit: resolve the viewport, enforce the final budget, and
        // render exactly once.
        this.refreshPinnedSegments();
        this.enforceBudget();
        adapter?.render?.(this);
        if (anchor) {
          const top = targetID == null ? null : adapter?.topForID?.(targetID);
          if (Number.isFinite(top) && Number.isFinite(anchor.top)) adapter?.adjustScroll?.(top - anchor.top);
        }
        return result;
      };

      const result = mutation();
      if (result && typeof result.then === 'function') {
        return Promise.resolve(result).then(finish);
      }
      return finish(result);
    }

    releaseBodies() {
      this.bodies.clear();
      for (const segment of this.segments) this.refreshSegmentState(segment);
    }

    rekey(sessionId) {
      const next = String(sessionId || '');
      if (!next || next === this.sessionId) return this;
      if (storesBySession.get(this.sessionId) === this) storesBySession.delete(this.sessionId);
      this.sessionId = next;
      storesBySession.set(this.sessionId, this);
      return this;
    }

    destroy() {
      this.releaseBodies();
      if (storesBySession.get(this.sessionId) === this) storesBySession.delete(this.sessionId);
    }

    _checkInvariants() {
      const length = this.ids.length;
      if (this.seqs.length !== length || this.flags.length !== length || this.roles.length !== length) {
        throw new Error('transcript skeleton arrays differ in length');
      }
      if (new Set(this.ids).size !== length) throw new Error('transcript skeleton IDs are not unique');
      if (length === 0 && this.segments.length !== 0) throw new Error('empty transcript has segments');
      let nextOrdinal = 0;
      for (const [index, segment] of this.segments.entries()) {
        if (segment.startOrdinal !== nextOrdinal || segment.endOrdinal < segment.startOrdinal) {
          throw new Error(`segment ${index} does not partition transcript`);
        }
        if (index > 0 && this.roles[segment.startOrdinal] !== 'u') {
          throw new Error(`segment ${index} does not begin with a user row`);
        }
        nextOrdinal = segment.endOrdinal + 1;
        const required = this.requiredBodyIDs(index);
        const complete = required.length > 0 && required.every((id) => this.bodies.has(id));
        if (required.length === 0) {
          if (segment.state !== 'empty') throw new Error(`segment ${index} with no display rows is not stable-empty`);
        } else if ((segment.state === 'materialized') !== complete) {
          throw new Error(`segment ${index} has partial materialization`);
        }
        if (segment.state === 'evicted') {
          for (let ordinal = segment.startOrdinal; ordinal <= segment.endOrdinal; ordinal += 1) {
            if (this.bodies.has(this.ids[ordinal])) throw new Error(`evicted segment ${index} retains a body`);
          }
        }
      }
      if (nextOrdinal !== length) throw new Error('segments do not cover transcript');
      for (const id of this.bodies.keys()) {
        if (!this.ids.includes(id)) throw new Error('body cache contains retired durable ID');
      }
      const materialized = this.segments.reduce((count, segment) => count + (segment.state === 'materialized' ? 1 : 0), 0);
      const pinnedMaterialized = [...this.pinnedSegments].reduce((count, index) => count + (this.segments[index]?.state === 'materialized' ? 1 : 0), 0);
      if (materialized > this.budgets.maxMaterializedTurns && materialized > pinnedMaterialized) {
        throw new Error(`materialized turn budget exceeded: ${materialized}`);
      }
      const runs = this.renderRuns();
      for (let i = 1; i < runs.length; i += 1) {
        if (runs[i - 1].type === 'gap' && runs[i].type === 'gap') throw new Error('adjacent gaps were not coalesced');
      }
      return true;
    }
  }

  const transcriptStoreFromMessages = (sessionId, payload, budgets = {}) => {
    const pages = Array.isArray(payload) ? payload : [payload || {}];
    const messages = [];
    let compactionSeq = -1;
    let compactionCount = 0;
    for (const page of pages) {
      if (Array.isArray(page?.messages)) messages.push(...page.messages);
      if (Number.isFinite(Number(page?.compaction_seq))) compactionSeq = finiteInt(page.compaction_seq, -1);
      if (Number.isFinite(Number(page?.compaction_count))) compactionCount = finiteInt(page.compaction_count, 0);
    }
    messages.sort((a, b) => bodySeq(a) - bodySeq(b) || Number(bodyID(a)) - Number(bodyID(b)));
    const unique = [];
    const seen = new Set();
    for (const message of messages) {
      const id = bodyID(message);
      if (seen.has(id)) continue;
      seen.add(id);
      unique.push(message);
    }
    const store = new TranscriptStore(sessionId, budgets);
    store.applyIndex({
      rev: 0,
      compaction_seq: compactionSeq,
      compaction_count: compactionCount,
      rows: {
        ids: unique.map(bodyID),
        seqs: unique.map(bodySeq),
        roles: unique.map((message) => roleCode(message.role)).join(''),
        flags: unique.map((message) => {
          let flags = message.compaction_tail ? TRANSCRIPT_FLAG_COMPACTION_TAIL : 0;
          if (!Array.isArray(message.parts) || message.parts.length === 0) flags |= TRANSCRIPT_FLAG_EMPTY_BODY;
          return flags;
        })
      }
    });
    store.materialize(unique, { countFetch: false });
    return store;
  };

  const __transcriptStats = (sessionId) => {
    const store = storesBySession.get(String(sessionId || ''));
    return store ? Object.assign({}, store.stats) : null;
  };
  root.__transcriptStats = __transcriptStats;

  return {
    TranscriptStore,
    TRANSCRIPT_BUDGETS,
    TRANSCRIPT_MATERIALIZE_BATCH_TURNS,
    TRANSCRIPT_FLAG_COMPACTION_TAIL,
    TRANSCRIPT_FLAG_EMPTY_BODY,
    transcriptStoreFromMessages,
    transcriptRoleCode: roleCode,
    transcriptRoleName: roleName,
    __transcriptStats
  };
});
