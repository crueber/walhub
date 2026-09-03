// web/src/index.jsx — the SolidJS entry (D-WEB-6): one client, one router,
// the same route table the vanilla SPA had. The SDK default client is the
// dogfood data path (§6); initData wires it into the data layer.

import { render } from "solid-js/web";
import "./ui.css";
import { Router, Route } from "@solidjs/router";

import repos from "../sdk/src/index.js";
import App from "./App.jsx";
import { initData } from "./lib/data.js";

import Owners from "./pages/Owners.jsx";
import Repos from "./pages/Repos.jsx";
import Repo from "./pages/Repo.jsx";
import Tree from "./pages/Tree.jsx";
import Blob from "./pages/Blob.jsx";
import Commits from "./pages/Commits.jsx";
import Commit from "./pages/Commit.jsx";
import Wal from "./pages/Wal.jsx";
import Settings from "./pages/Settings.jsx";
import Apidocs from "./pages/Apidocs.jsx";
import Setup from "./pages/Setup.jsx";
import Keys from "./pages/Keys.jsx";

initData(repos); // the dogfood client, one instance

render(
  () => (
    <Router root={App}>
      <Route path="/" component={Owners} />
      <Route path="/setup" component={Setup} />
      <Route path="/api" component={Apidocs} />
      <Route path="/keys" component={Keys} />
      <Route path="/:owner" component={Repos} />
      <Route path="/:owner/:name" component={Repo}>
        <Route path="/" component={Tree} />
        <Route path="/tree/*rest" component={Tree} />
        <Route path="/blob/*rest" component={Blob} />
        <Route path="/commits" component={Commits} />
        <Route path="/commit/:sha" component={Commit} />
        <Route path="/wal" component={Wal} />
        <Route path="/settings" component={Settings} />
      </Route>
      <Route path="*" component={Owners} />
    </Router>
  ),
  document.getElementById("root"),
);
