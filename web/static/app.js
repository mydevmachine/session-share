(function () {
  "use strict";

  var body = document.body;
  var shareID = body.dataset.id;
  var mode = body.dataset.mode;
  var expires = new Date(body.dataset.expires);

  var FINAL = {
    4001: "This share expired.",
    4002: "The owner stopped sharing.",
    4003: "The shared session ended.",
    4004: "You are not signed in to this share. Reload the page.",
    4005: "Someone is already watching, and this share allows only one viewer."
  };

  var events = [];
  var connID = "";
  var attempt = 0;
  var finished = false;
  var socket = null;

  function note(kind, detail) {
    events.push({ at: new Date().toISOString(), kind: kind, detail: detail || "" });
    if (events.length > 200) events.shift();
  }

  var term = new Terminal({
    cursorBlink: mode === "write",
    scrollback: 5000,
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
    fontSize: 13,
    theme: { background: "#0f1115" }
  });
  var holder = document.getElementById("terminal");
  var stage = document.getElementById("stage");
  term.open(stage);
  term.focus();

  function load(key) {
    try { return localStorage.getItem("session-share." + key); } catch (e) { return null; }
  }
  function save(key, value) {
    try { localStorage.setItem("session-share." + key, value); } catch (e) {}
  }

  var view = load("view") || "fit";
  var zoom = parseFloat(load("zoom")) || 1;

  // The guest sees the shared window at its own size, never more or fewer
  // columns: tmux fills a bigger client with dots and cuts a smaller one.
  // The terminal is scaled instead: the whole screen, the full width, or its
  // actual size, times the guest's own zoom.
  function fitToWindow() {
    var el = term.element;
    var screen = el && el.querySelector(".xterm-screen");
    if (!screen) return;
    var width = screen.offsetWidth, height = screen.offsetHeight;
    if (!width || !height) return;
    var room = { w: holder.clientWidth - 8, h: holder.clientHeight - 8 };
    var base = view === "width" ? room.w / width
      : view === "actual" ? 1
      : Math.min(room.w / width, room.h / height);
    var scale = Math.max(0.25, Math.min(4, base * zoom));
    el.style.width = width + "px";
    el.style.height = height + "px";
    el.style.transform = "scale(" + scale + ")";
    stage.style.width = Math.floor(width * scale) + "px";
    stage.style.height = Math.floor(height * scale) + "px";
    holder.classList.toggle("scrolls", width * scale > room.w + 1 || height * scale > room.h + 1);
    document.querySelectorAll("[data-view]").forEach(function (b) {
      b.classList.toggle("on", b.dataset.view === view);
    });
  }

  document.querySelectorAll("[data-view]").forEach(function (button) {
    button.addEventListener("click", function () {
      view = button.dataset.view;
      zoom = 1;
      save("view", view);
      save("zoom", zoom);
      fitToWindow();
      term.focus();
    });
  });
  function step(factor) {
    zoom = Math.max(0.25, Math.min(4, zoom * factor));
    save("zoom", zoom);
    fitToWindow();
    term.focus();
  }
  document.getElementById("zoom-in").addEventListener("click", function () { step(1.1); });
  document.getElementById("zoom-out").addEventListener("click", function () { step(1 / 1.1); });

  function useSize(cols, rows) {
    if (!cols || !rows) return;
    if (term.cols !== cols || term.rows !== rows) term.resize(cols, rows);
    requestAnimationFrame(fitToWindow);
  }

  var statusEl = document.getElementById("status");
  var statusText = document.getElementById("status-text");

  function setStatus(state, text) {
    statusEl.className = "status " + state;
    statusText.textContent = text;
  }

  function showOverlay(text) {
    document.getElementById("overlay-text").textContent = text;
    document.getElementById("overlay").hidden = false;
  }

  function send(msg) {
    if (socket && socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(msg));
  }

  function connect() {
    var scheme = location.protocol === "https:" ? "wss://" : "ws://";
    socket = new WebSocket(scheme + location.host + "/s/" + shareID + "/ws");
    socket.binaryType = "arraybuffer";
    setStatus("", attempt ? "reconnecting" : "connecting");
    note("connecting", "attempt " + attempt);

    socket.onopen = function () {
      attempt = 0;
      setStatus("ok", "live");
      note("open");
    };

    socket.onmessage = function (ev) {
      if (typeof ev.data === "string") {
        var msg = JSON.parse(ev.data);
        if (msg.type === "hello") {
          connID = msg.conn_id;
          expires = new Date(msg.expires_at);
          note("hello", connID);
          useSize(msg.cols, msg.rows);
        } else if (msg.type === "size") {
          note("size", msg.cols + "x" + msg.rows);
          useSize(msg.cols, msg.rows);
        } else if (msg.type === "chat-history") {
          chat.history(msg.messages || []);
        } else if (msg.type === "chat") {
          chat.add(msg.message);
        } else if (msg.type === "chat-refused") {
          chat.refused(msg.reason);
        }
        return;
      }
      term.write(new Uint8Array(ev.data));
    };

    socket.onclose = function (ev) {
      note("close", ev.code + " " + (ev.reason || ""));
      if (FINAL[ev.code]) {
        finished = true;
        setStatus("bad", "closed");
        showOverlay(ev.reason || FINAL[ev.code]);
        return;
      }
      if (finished) return;
      attempt += 1;
      var wait = Math.min(30, Math.pow(2, attempt - 1));
      setStatus("bad", "connection lost, retrying in " + wait + "s");
      setTimeout(connect, wait * 1000);
    };

    socket.onerror = function () {
      note("error");
    };
  }

  term.onData(function (data) {
    send({ type: "input", data: data });
  });

  if (window.ResizeObserver) {
    new ResizeObserver(function () { fitToWindow(); }).observe(holder);
  } else {
    window.addEventListener("resize", fitToWindow);
  }

  function pad(n) { return n < 10 ? "0" + n : "" + n; }

  setInterval(function () {
    var left = Math.max(0, Math.floor((expires - new Date()) / 1000));
    var h = Math.floor(left / 3600), m = Math.floor((left % 3600) / 60), s = left % 60;
    document.getElementById("countdown").textContent = (h ? h + ":" + pad(m) : m) + ":" + pad(s) + " left";
  }, 1000);

  document.getElementById("diag").addEventListener("click", function () {
    var report = JSON.stringify({
      share_id: shareID,
      conn_id: connID,
      page_loaded: performance.timeOrigin ? new Date(performance.timeOrigin).toISOString() : "",
      user_agent: navigator.userAgent,
      events: events
    }, null, 2);
    var button = this;
    navigator.clipboard.writeText(report).then(function () {
      button.textContent = "Copied";
      setTimeout(function () { button.textContent = "Copy diagnostics"; }, 2000);
    });
  });

  var chat = (function () {
    var box = document.getElementById("messages");
    var form = document.getElementById("chat-form");
    var nameInput = document.getElementById("chat-name");
    var textInput = document.getElementById("chat-text");
    var noteEl = document.getElementById("chat-note");
    var unreadEl = document.getElementById("unread");
    var panel = document.getElementById("chat");
    var divider = document.getElementById("divider");
    var unread = 0;
    var sentIDs = {};

    nameInput.value = load("chat.name") || "";
    nameInput.hidden = !!nameInput.value;
    var width = parseInt(load("chat.width"), 10);
    if (width) panel.style.width = width + "px";
    if (load("chat.open") === "0") document.body.classList.add("chat-hidden");

    function isOpen() { return !document.body.classList.contains("chat-hidden"); }

    function showUnread() {
      unreadEl.hidden = unread === 0;
      unreadEl.textContent = unread > 9 ? "9+" : String(unread);
    }

    document.getElementById("chat-toggle").addEventListener("click", function () {
      document.body.classList.toggle("chat-hidden");
      save("chat.open", isOpen() ? "1" : "0");
      if (isOpen()) { unread = 0; showUnread(); textInput.focus(); } else { term.focus(); }
    });

    divider.addEventListener("pointerdown", function (ev) {
      divider.setPointerCapture(ev.pointerId);
      divider.classList.add("dragging");
      function move(e) {
        var w = Math.max(240, Math.min(window.innerWidth * 0.6, window.innerWidth - e.clientX));
        panel.style.width = w + "px";
      }
      function up(e) {
        divider.releasePointerCapture(e.pointerId);
        divider.classList.remove("dragging");
        divider.removeEventListener("pointermove", move);
        divider.removeEventListener("pointerup", up);
        save("chat.width", parseInt(panel.style.width, 10));
      }
      divider.addEventListener("pointermove", move);
      divider.addEventListener("pointerup", up);
    });

    function clock(ts) {
      var d = new Date(ts);
      return (d.getHours() < 10 ? "0" : "") + d.getHours() + ":" + (d.getMinutes() < 10 ? "0" : "") + d.getMinutes();
    }

    function render(m) {
      var el = document.createElement("div");
      var mine = m.role === "guest" && m.from === (load("chat.name") || nameInput.value);
      el.className = "msg " + (m.role === "owner" ? "owner" : mine ? "mine" : "guest");
      var meta = document.createElement("span");
      meta.className = "meta";
      meta.textContent = (m.role === "owner" ? m.from + " (owner)" : m.from) + " · " + clock(m.ts);
      var text = document.createElement("span");
      text.textContent = m.text;
      el.appendChild(meta);
      el.appendChild(text);
      return el;
    }

    function add(m) {
      if (!m || !m.id) return;
      var nearBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
      box.appendChild(render(m));
      if (nearBottom || sentIDs[m.text]) box.scrollTop = box.scrollHeight;
      delete sentIDs[m.text];
      if (!isOpen()) { unread += 1; showUnread(); }
    }

    form.addEventListener("submit", function (ev) {
      ev.preventDefault();
      var name = nameInput.value.trim();
      var text = textInput.value.trim();
      if (!name) { nameInput.hidden = false; nameInput.focus(); return; }
      if (!text) return;
      save("chat.name", name);
      nameInput.hidden = true;
      sentIDs[text] = true;
      send({ type: "chat", name: name, data: text });
      textInput.value = "";
      noteEl.hidden = true;
    });

    return {
      history: function (msgs) {
        box.textContent = "";
        msgs.forEach(function (m) { box.appendChild(render(m)); });
        box.scrollTop = box.scrollHeight;
      },
      add: add,
      refused: function (reason) {
        noteEl.textContent = reason || "Not sent.";
        noteEl.hidden = false;
      }
    };
  })();

  connect();
})();
