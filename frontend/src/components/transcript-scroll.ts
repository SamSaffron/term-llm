export const TRANSCRIPT_SCROLL_TO_TAIL_EVENT = 'term-llm:scroll-transcript-to-tail';

export function requestTranscriptScrollToTail(): void {
  document.getElementById('chatScroll')?.dispatchEvent(new Event(TRANSCRIPT_SCROLL_TO_TAIL_EVENT));
}
