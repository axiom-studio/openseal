import { Routes, Route, NavLink } from 'react-router-dom';
import Workflows from './pages/Workflows';
import Runs from './pages/Runs';
import Execution from './pages/Execution';
import Skills from './pages/Skills';

function App() {
  return (
    <div className="app">
      <nav className="sidebar">
        <div className="brand">
          <div className="brand-mark">◈</div>
          <div className="brand-text">OpenSeal</div>
        </div>
        <div className="nav-links">
          <NavLink to="/" className={({ isActive }) => (isActive ? 'active' : '')} end>
            <span className="nav-icon">▣</span>
            <span>Workflows</span>
          </NavLink>
          <NavLink to="/runs" className={({ isActive }) => (isActive ? 'active' : '')}>
            <span className="nav-icon">▤</span>
            <span>Runs</span>
          </NavLink>
          <NavLink to="/skills" className={({ isActive }) => (isActive ? 'active' : '')}>
            <span className="nav-icon">▦</span>
            <span>Skills</span>
          </NavLink>
        </div>
        <div className="sidebar-footer">
          <div className="status-dot" />
          <span>System Online</span>
        </div>
      </nav>
      <main className="content">
        <Routes>
          <Route path="/" element={<Workflows />} />
          <Route path="/runs" element={<Runs />} />
          <Route path="/runs/:id" element={<Execution />} />
          <Route path="/skills" element={<Skills />} />
        </Routes>
      </main>
    </div>
  );
}

export default App;
