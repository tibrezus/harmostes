import { useEffect, useState } from "react";
import { PipelineList } from "./pages/PipelineList";
import { PipelineEditor } from "./pages/PipelineEditor";
import { WorkflowCanvas } from "./pages/WorkflowCanvas";

// Simple path-based router. The Go server serves this SPA at:
//   /pipelines              → list page
//   /pipelines/new          → new pipeline editor
//   /pipelines/{name}       → existing pipeline editor
//   /workflows/{name}/canvas → read-only workflow canvas (compiled graph)

type Route =
  | { page: "list" }
  | { page: "editor"; name?: string }
  | { page: "workflow-canvas"; name: string };

function parseRoute(): Route {
  const path = window.location.pathname.replace(/\/+$/, "");

  // Workflow canvas (read-only compiled graph from a Workflow CR)
  const wfCanvasMatch = path.match(/^\/workflows\/(.+)\/canvas$/);
  if (wfCanvasMatch) {
    return { page: "workflow-canvas", name: decodeURIComponent(wfCanvasMatch[1]) };
  }

  // Pipeline routes (editable graph editor)
  if (path === "/pipelines" || path === "/pipelines/") {
    return { page: "list" };
  }
  if (path === "/pipelines/new") {
    return { page: "editor" };
  }
  const match = path.match(/^\/pipelines\/(.+)$/);
  if (match) {
    return { page: "editor", name: decodeURIComponent(match[1]) };
  }
  return { page: "list" };
}

export default function App() {
  const [route, setRoute] = useState(parseRoute());

  useEffect(() => {
    const onPop = () => setRoute(parseRoute());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  switch (route.page) {
    case "workflow-canvas":
      return <WorkflowCanvas name={route.name} />;
    case "editor":
      return <PipelineEditor name={route.name} />;
    default:
      return <PipelineList />;
  }
}
