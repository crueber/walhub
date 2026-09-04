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
import Issues from "./pages/Issues.jsx";
import IssueNew from "./pages/IssueNew.jsx";
import Issue from "./pages/Issue.jsx";
import Pulls from "./pages/Pulls.jsx";
import Pull from "./pages/Pull.jsx";
import Checks from "./pages/Checks.jsx";
import Releases from "./pages/Releases.jsx";
import Release from "./pages/Release.jsx";
import ReleaseNew from "./pages/ReleaseNew.jsx";
import Notifications from "./pages/Notifications.jsx";
import Labels from "./pages/Labels.jsx";
import Milestones from "./pages/Milestones.jsx";
import Wal from "./pages/Wal.jsx";
import Settings from "./pages/Settings.jsx";
import Org from "./pages/Org.jsx";
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
      <Route path="/notifications" component={Notifications} />
      <Route path="/:owner" component={Repos} />
      <Route path="/:org/settings" component={Org} />
      <Route path="/:owner/:name" component={Repo}>
        <Route path="/" component={Tree} />
        <Route path="/tree/*rest" component={Tree} />
        <Route path="/blob/*rest" component={Blob} />
        <Route path="/commits" component={Commits} />
        <Route path="/commit/:sha" component={Commit} />
        <Route path="/issues" component={Issues} />
        <Route path="/issues/new" component={IssueNew} />
        <Route path="/issues/:num" component={Issue} />
        <Route path="/pulls" component={Pulls} />
        <Route path="/pull/:num" component={Pull} />
        <Route path="/checks" component={Checks} />
        <Route path="/releases" component={Releases} />
        <Route path="/releases/new" component={ReleaseNew} />
        <Route path="/releases/:tag" component={Release} />
        <Route path="/labels" component={Labels} />
        <Route path="/milestones" component={Milestones} />
        <Route path="/wal" component={Wal} />
        <Route path="/settings" component={Settings} />
      </Route>
      <Route path="*" component={Owners} />
    </Router>
  ),
  document.getElementById("root"),
);
