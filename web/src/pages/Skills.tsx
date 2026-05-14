import { useEffect, useState } from 'react';
import { api, type ExecutorInfo } from '../api/client';

const categories = ['all', 'trigger', 'control', 'action', 'data'];

function categoryClass(cat: string) {
  switch (cat) {
    case 'trigger': return 'cat-trigger';
    case 'control': return 'cat-control';
    case 'action': return 'cat-action';
    case 'data': return 'cat-data';
    default: return 'cat-other';
  }
}

export default function Skills() {
  const [skills, setSkills] = useState<ExecutorInfo[]>([]);
  const [filtered, setFiltered] = useState<ExecutorInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [activeCategory, setActiveCategory] = useState('all');
  const [search, setSearch] = useState('');

  useEffect(() => {
    api.listSkills()
      .then((data) => {
        setSkills(data);
        setFiltered(data);
      })
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, []);

  useEffect(() => {
    let result = skills;
    if (activeCategory !== 'all') {
      result = result.filter((s) => s.category === activeCategory);
    }
    if (search.trim()) {
      const q = search.toLowerCase();
      result = result.filter(
        (s) =>
          s.name.toLowerCase().includes(q) ||
          s.description.toLowerCase().includes(q) ||
          s.type.toLowerCase().includes(q)
      );
    }
    setFiltered(result);
  }, [activeCategory, search, skills]);

  if (loading) return <div className="page-loading">Loading skills...</div>;
  if (error) return <div className="page-error">Error: {error}</div>;

  return (
    <div className="page">
      <header className="page-header">
        <h1>Skills</h1>
        <p className="page-subtitle">{skills.length} executors available</p>
      </header>

      <div className="skills-controls">
        <input
          className="search-input"
          type="text"
          placeholder="Search skills..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <div className="category-filters">
          {categories.map((cat) => (
            <button
              key={cat}
              className={`filter-btn ${activeCategory === cat ? 'active' : ''}`}
              onClick={() => setActiveCategory(cat)}
            >
              {cat}
            </button>
          ))}
        </div>
      </div>

      <div className="grid skills-grid">
        {filtered.map((skill) => (
          <div className="card skill-card" key={skill.type}>
            <div className="skill-card-header">
              <div className="skill-icon">{skill.icon || '◈'}</div>
              <span className={`category-pill ${categoryClass(skill.category)}`}>
                {skill.category}
              </span>
            </div>
            <div className="skill-card-body">
              <div className="skill-name">{skill.name || skill.type}</div>
              <div className="skill-type">{skill.type}</div>
              <div className="skill-description">{skill.description}</div>
            </div>
          </div>
        ))}
      </div>

      {filtered.length === 0 && (
        <div className="empty-state">No skills match your filters.</div>
      )}
    </div>
  );
}
