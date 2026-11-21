# Replay and video export

Open **Sessions**, then **Replay**. Replay reads only the database. It costs
no credits, and a browser in replay does not keep the live poller running.
Replay is local to your browser; others stay on the live session.

- Play, pause, scrub, speeds from 1× to 120×, and jump to the previous or
  next recorded event.
- Aircraft are interpolated along a great circle between the two snapshots
  either side of the playhead. Aircraft in only one of the two fade in or
  out.
- Camera: **As recorded** follows the recorded camera, **Free** lets you fly,
  **Follow aircraft** tracks the aircraft you clicked, **Keyframes** moves
  between cameras you placed with **Add keyframe**.
- For a viewport session, a keyframe that looks outside the recorded area is
  marked red and warned about.

**Export video** renders the replay to MP4 in the browser, frame by frame. It
waits for imagery on every frame, so the result is the same on a slow machine
as on a fast one. Options: time range, speed, resolution up to 4K, frame
rate, camera, scope outline, and burned-in clock and attribution.

Export needs WebCodecs (current Chrome, Edge, Safari). Without it the dialog
says so; there is no lower-quality fallback. Chrome and Edge encode H.264.
A browser with no H.264 encoder gets VP9 or AV1 in the MP4 instead, and the
dialog says which.
