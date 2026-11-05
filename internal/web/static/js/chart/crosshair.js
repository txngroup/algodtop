// The crosshair bus synchronizes hover across every chart on a page: the
// chart under the pointer publishes a time, and every chart draws its
// hairline and legend values at that instant. It also carries zoom
// requests, so dragging on any chart zooms all of them.

export class Bus {
  constructor() {
    this.subs = new Set();
    this.zoomSubs = new Set();
    this.t = null;
    this.source = null;
    this.frame = 0;
  }

  // on subscribes to crosshair moves: fn(t, source). It returns an
  // unsubscribe function.
  on(fn) {
    this.subs.add(fn);
    return () => this.subs.delete(fn);
  }

  // move publishes a crosshair time (or null). Updates are coalesced to
  // one per animation frame, which keeps hover cheap with many charts.
  move(t, source) {
    this.t = t;
    this.source = source;
    if (this.frame) return;
    this.frame = requestAnimationFrame(() => {
      this.frame = 0;
      for (const fn of this.subs) fn(this.t, this.source);
    });
  }

  onZoom(fn) {
    this.zoomSubs.add(fn);
    return () => this.zoomSubs.delete(fn);
  }

  zoom(from, to) {
    for (const fn of this.zoomSubs) fn(from, to);
  }
}
